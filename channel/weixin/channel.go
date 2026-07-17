package weixin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/logger"
	"gopkg.in/yaml.v3"
)

func init() {
	channel.Register("weixin", func(rawCfg map[string]any) (channel.Channel, error) {
		b, err := yaml.Marshal(rawCfg)
		if err != nil {
			return nil, fmt.Errorf("weixin: marshal config: %w", err)
		}
		var cfg config.WeixinConfig
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("weixin: unmarshal config: %w", err)
		}
		return NewChannel(cfg)
	})
}

// Channel implements the channel.Channel interface for WeChat iLink Bot.
type Channel struct {
	cfg           config.WeixinConfig
	store         *stateStore
	cli           *client
	typingTickets *typingTicketCache

	// Resolved at login time.
	accountID string // ilink_bot_id, e.g. "a1b2c3d4@im.bot"
	userID    string // ilink_user_id, the scanner's WeChat ID
	botToken  string // bearer token for API calls

	// Greeting message sent to the admin user after login.
	// Set via OnStart() from config; empty = no greeting.
	greeting string

	// greetingSent is set to true after the startup greeting has been
	// delivered (either in OnStart or Run), to prevent duplicate sends.
	greetingSent bool

	logger *logger.Logger
}

// NewChannel creates a Weixin channel. It validates config, initialises the
// state store, and prepares an HTTP client. Login (QR scan) is deferred to
// Run().
func NewChannel(cfg config.WeixinConfig) (*Channel, error) {
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = defaultStateDir()
	}

	store, err := newStateStore(stateDir)
	if err != nil {
		return nil, fmt.Errorf("weixin: state store: %w", err)
	}

	cli := newClient()
	return &Channel{
		cfg:           cfg,
		store:         store,
		cli:           cli,
		typingTickets: newTypingTicketCache(cli),
		logger:        logger.New("channel.weixin"),
	}, nil
}

// Name returns the channel type identifier.
func (ch *Channel) Name() string { return "weixin" }

// Send implements channel.MessageSender for proactive message delivery
// (used by cron job triggers). It parses the ThreadID into accountID and
// userID, looks up the context token, and sends text + any file attachments.
func (ch *Channel) Send(ctx context.Context, msg channel.OutgoingMessage) error {
	// Parse ThreadID: accountID:userID
	parts := splitThreadID(msg.ThreadID)
	if len(parts) != 2 {
		return fmt.Errorf("weixin: invalid ThreadID %q for Send", msg.ThreadID)
	}
	accountID := parts[0]
	userID := parts[1]

	// Verify this is our account.
	if accountID != ch.accountID {
		return fmt.Errorf("weixin: ThreadID account %q does not match our account %q", accountID, ch.accountID)
	}

	// Load context token.
	contextToken := ch.store.loadContextToken(accountID, userID)

	// Send text content if present.
	if msg.Content != "" {
		if err := ch.sendTextReply(userID, contextToken, msg.Content); err != nil {
			return fmt.Errorf("weixin: Send text: %w", err)
		}
	}

	// Send each attachment as a separate media message.
	// Supports both inline Data and deferred LocalPath (read from disk at send time).
	for _, att := range msg.Attachments {
		mediaType := channelAttachmentToILinkMediaType(att.Type)
		data, err := channel.ResolveAttachmentData(att)
		if err != nil {
			ch.logger.Error(ctx, "weixin: Send resolve attachment", err, "file", att.FileName)
			continue
		}
		if err := ch.sendMediaReply(userID, contextToken, data, att.FileName, mediaType); err != nil {
			ch.logger.Error(ctx, "weixin: Send attachment error", err, "file", att.FileName)
		}
	}

	return nil
}

// splitThreadID splits a ThreadID of the form "accountID:userID".
func splitThreadID(threadID string) []string {
	idx := strings.LastIndex(threadID, ":")
	if idx < 0 {
		return nil
	}
	return []string{threadID[:idx], threadID[idx+1:]}
}

// Run starts the channel's message loop. It first attempts to load stored
// credentials; if none exist, it performs a QR-code login flow. Then it
// enters the long-polling receive loop.
func (ch *Channel) Run(ctx context.Context, handler channel.MessageHandler) error {
	ch.cli.SetRouteTag(ch.cfg.RouteTag)
	ch.cli.SetBotAgent(ch.cfg.BotAgent)

	// --- Load existing account or login ---
	if err := ch.loadOrLogin(ctx); err != nil {
		if ctx.Err() != nil {
			return nil // cancelled during login
		}
		return fmt.Errorf("weixin: login: %w", err)
	}

	ch.cli.SetBotToken(ch.botToken)

	ch.logger.Info(ctx, "weixin: logged in", "account", ch.accountID, "bot_token_prefix", ch.botToken[:8]+"...", "user", ch.userID)
	fmt.Printf("[weixin] logged in as %s\n", ch.accountID)

	// Send startup greeting if it wasn't already sent in OnStart.
	// This covers the first-time setup path where no saved account
	// existed at OnStart time — the greeting goes out right after
	// the QR-code login completes.
	if !ch.greetingSent && ch.greeting != "" && ch.userID != "" {
		ch.logger.Info(ctx, "weixin: sending startup greeting", "user", ch.userID)
		if err := ch.sendTextReply(ch.userID, "", ch.greeting); err != nil {
			// Non-fatal: log and continue.
			ch.logger.Error(ctx, "weixin: greeting send error", err)
		}
	}

	// Notify server that this channel client is starting (v2.1.10+).
	ch.logger.Info(ctx, "weixin: sending notifyStart")
	if resp, err := ch.cli.notifyStart(); err != nil {
		ch.logger.Warn(ctx, "weixin: notifyStart error (ignored)", "error", err)
	} else if resp.Ret != 0 {
		ch.logger.Warn(ctx, "weixin: notifyStart", "ret", resp.Ret, "errmsg", resp.ErrMsg)
	}

	// Ensure notifyStop is sent when Run exits for any reason.
	defer func() {
		ch.logger.Info(ctx, "weixin: sending notifyStop")
		if resp, err := ch.cli.notifyStop(); err != nil {
			ch.logger.Warn(ctx, "weixin: notifyStop error (ignored)", "error", err)
		} else if resp.Ret != 0 {
			ch.logger.Warn(ctx, "weixin: notifyStop", "ret", resp.Ret, "errmsg", resp.ErrMsg)
		}
	}()

	// --- Long-polling loop ---
	return ch.pollingLoop(ctx, handler)
}

// OnStart implements channel.Channel. It resolves the greeting message
// from config and, if a previously-saved account exists, sends the
// greeting immediately — without waiting for Run() to complete login.
//
// When no saved account is available (first-time setup), the greeting
// is deferred to Run() and sent after the QR-code login succeeds.
func (ch *Channel) OnStart(ctx context.Context) error {
	// Resolve greeting message. Empty = no greeting.
	ch.greeting = ch.cfg.Greeting

	// If a saved account exists, we already know the admin user's ID and
	// can send the startup greeting right away, before entering Run().
	accounts, err := ch.store.loadAccountList()
	if err != nil || len(accounts) == 0 {
		return nil // no saved account; greeting will be sent in Run() after login
	}

	data, err := ch.store.loadAccount(accounts[0])
	if err != nil {
		ch.logger.Error(ctx, "weixin: OnStart: load account", err)
		return nil // non-fatal
	}

	// Set up the client so we can send the greeting.
	ch.accountID = accounts[0]
	ch.userID = data.UserID
	ch.botToken = data.Token
	ch.cli.SetBaseURL(data.BaseURL)
	ch.cli.SetBotToken(ch.botToken)
	ch.cli.SetRouteTag(ch.cfg.RouteTag)

	if ch.greeting != "" {
		ch.logger.Info(ctx, "weixin: sending startup greeting", "user", ch.userID)
		if err := ch.sendTextReply(ch.userID, "", ch.greeting); err != nil {
			ch.logger.Error(ctx, "weixin: greeting send error", err)
		}
	}
	ch.greetingSent = true
	return nil
}

// loadOrLogin attempts to load a previously stored account, falling back to
// interactive QR-code login.
func (ch *Channel) loadOrLogin(ctx context.Context) error {
	accounts, err := ch.store.loadAccountList()
	if err != nil {
		return err
	}

	if len(accounts) > 0 {
		// Load the first registered account.
		data, err := ch.store.loadAccount(accounts[0])
		if err != nil {
			return fmt.Errorf("load account %s: %w", accounts[0], err)
		}
		ch.accountID = accounts[0]
		ch.userID = data.UserID
		ch.botToken = data.Token
		ch.cli.SetBaseURL(data.BaseURL)

		// Ensure the configured user is in the allowlist (may be missing from
		// accounts created before this fix was applied).
		if !ch.store.isUserAllowed(ch.accountID, ch.userID) {
			ch.logger.Info(ctx, "weixin: auto-adding user to allowlist", "user", ch.userID)
			allowFrom := &AllowFromData{
				Version:   1,
				AllowFrom: []string{ch.userID},
			}
			ch.store.saveAllowFrom(ch.accountID, allowFrom)
		}

		ch.logger.Info(ctx, "weixin: loaded stored account", "account", accounts[0])
		return nil
	}

	// No stored account → start QR login.
	return ch.qrLogin(ctx)
}

// qrLogin performs the QR-code login flow: fetch QR → poll status until
// confirmed or cancelled.
//
// Supports v2.3.1+ protocol extensions:
//   - POST get_bot_qrcode with local_token_list for binded_redirect detection
//   - need_verifycode / verify_code_blocked pair-code flow
//   - binded_redirect for already-bound bot detection
func (ch *Channel) qrLogin(ctx context.Context) error {
	fmt.Println("[weixin] no stored account found. starting QR login...")
	fmt.Println("[weixin] open the following URL and scan with WeChat:")

	// Collect local bot tokens for binded_redirect detection.
	localTokens := ch.collectLocalBotTokens()
	qr, err := ch.cli.getBotQRCode(&QRLoginRequest{LocalTokenList: localTokens})
	if err != nil {
		return fmt.Errorf("get QR code: %w", err)
	}

	fmt.Printf("\n  %s\n\n", qr.QRCodeImgContent)
	fmt.Println("[weixin] waiting for scan... (timeout: 240s)")

	const overallTimeout = 240 * time.Second
	deadline := time.Now().Add(overallTimeout)
	refreshCount := 0
	const maxQRRefreshes = 3

	// Track the pending verify code for need_verifycode flow.
	var pendingVerifyCode string

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		status, err := ch.cli.getQRCodeStatus(qr.QRCode, pendingVerifyCode)
		if err != nil {
			return fmt.Errorf("poll QR status: %w", err)
		}

		switch status.Status {
		case QRStatusConfirmed:
			ch.accountID = status.ILinkBotID
			ch.userID = status.ILinkUserID
			ch.botToken = status.BotToken
			if status.BaseURL != "" {
				ch.cli.SetBaseURL(status.BaseURL)
			}

			// Persist credentials.
			data := &AccountData{
				Token:   ch.botToken,
				SavedAt: time.Now().Format(time.RFC3339),
				BaseURL: ch.cli.baseURL,
				UserID:  ch.userID,
			}
			if err := ch.store.saveAccount(ch.accountID, data); err != nil {
				return fmt.Errorf("save account: %w", err)
			}

			// Add the scanning user to the allowlist.
			allowFrom := &AllowFromData{
				Version:   1,
				AllowFrom: []string{ch.userID},
			}
			if err := ch.store.saveAllowFrom(ch.accountID, allowFrom); err != nil {
				ch.logger.Warn(ctx, "weixin: warning: failed to save allowFrom", "error", err)
			}

			// Clean up duplicate accounts for the same userId.
			ch.deduplicateAccounts()
			return nil

		case QRStatusWait:
			// Continue polling.

		case QRStatusScaned:
			// If we had a pending verify code, it was accepted.
			if pendingVerifyCode != "" {
				pendingVerifyCode = ""
			}
			fmt.Println("[weixin] scanned! waiting for confirmation on phone...")

		case QRStatusNeedVerifyCode:
			// Server requests a pair-code for verification.
			prompt := "输入手机微信显示的数字，以继续连接："
			if pendingVerifyCode != "" {
				// Previous code was rejected.
				prompt = "❌ 你输入的数字不匹配，请重新输入："
			}
			code, err := readStdinLine(prompt)
			if err != nil {
				return fmt.Errorf("read verify code: %w", err)
			}
			if code != "" {
				pendingVerifyCode = code
			}
			// Continue polling immediately — the verify_code will be sent
			// with the next getQRCodeStatus call.

		case QRStatusVerifyCodeBlocked:
			fmt.Println("[weixin] too many incorrect pair-code attempts, refreshing QR...")
			pendingVerifyCode = ""
			refreshCount++
			if refreshCount > maxQRRefreshes {
				return fmt.Errorf("verify code blocked after %d QR refreshes", maxQRRefreshes)
			}
			qr, err = ch.cli.getBotQRCode(&QRLoginRequest{LocalTokenList: localTokens})
			if err != nil {
				return fmt.Errorf("refresh QR code after blocked verify code: %w", err)
			}
			fmt.Printf("\n  %s\n\n", qr.QRCodeImgContent)

		case QRStatusBindedRedirect:
			// Bot is already bound to this client — load existing credentials.
			fmt.Println("[weixin] bot already connected, loading existing account...")
			return ch.loadExistingAccount(ctx)

		case QRStatusExpired:
			if refreshCount >= maxQRRefreshes {
				return fmt.Errorf("QR code expired after %d refreshes", maxQRRefreshes)
			}
			fmt.Println("[weixin] QR expired, refreshing...")
			pendingVerifyCode = ""
			qr, err = ch.cli.getBotQRCode(&QRLoginRequest{LocalTokenList: localTokens})
			if err != nil {
				return fmt.Errorf("refresh QR code: %w", err)
			}
			fmt.Printf("\n  %s\n\n", qr.QRCodeImgContent)
			refreshCount++

		case QRStatusScanedButRedirect:
			if status.RedirectHost != "" {
				ch.cli.SetBaseURL("https://" + status.RedirectHost)
				fmt.Printf("[weixin] redirecting to %s\n", ch.cli.baseURL)
			}
		}
	}

	return fmt.Errorf("QR login timed out after %v", overallTimeout)
}

// collectLocalBotTokens gathers bot tokens from stored accounts for the
// local_token_list parameter used in get_bot_qrcode (v2.3.1+).
// Returns up to 10 most recent tokens.
func (ch *Channel) collectLocalBotTokens() []string {
	accounts, err := ch.store.loadAccountList()
	if err != nil {
		return nil
	}
	// Collect tokens from newest accounts first.
	var tokens []string
	for i := len(accounts) - 1; i >= 0 && len(tokens) < 10; i-- {
		data, err := ch.store.loadAccount(accounts[i])
		if err != nil || data.Token == "" {
			continue
		}
		tokens = append(tokens, data.Token)
	}
	return tokens
}

// loadExistingAccount loads the first stored account after binded_redirect.
// This handles the case where the bot is already bound but credentials
// exist locally.
func (ch *Channel) loadExistingAccount(_ context.Context) error {
	accounts, err := ch.store.loadAccountList()
	if err != nil {
		return fmt.Errorf("load accounts after binded_redirect: %w", err)
	}
	if len(accounts) == 0 {
		return fmt.Errorf("binded_redirect but no stored accounts found")
	}
	data, err := ch.store.loadAccount(accounts[0])
	if err != nil {
		return fmt.Errorf("load account %s after binded_redirect: %w", accounts[0], err)
	}
	ch.accountID = accounts[0]
	ch.userID = data.UserID
	ch.botToken = data.Token
	ch.cli.SetBaseURL(data.BaseURL)
	ch.logger.Info(context.Background(), "weixin: loaded existing account after binded_redirect", "account", accounts[0])
	return nil
}

// deduplicateAccounts removes old accounts that share the same userId as the
// current one, keeping only the newest.
func (ch *Channel) deduplicateAccounts() {
	accounts, err := ch.store.loadAccountList()
	if err != nil {
		return
	}
	for _, aid := range accounts {
		if aid == ch.accountID {
			continue
		}
		data, err := ch.store.loadAccount(aid)
		if err != nil {
			continue
		}
		if data.UserID == ch.userID {
			ch.logger.Info(context.Background(), "weixin: removing duplicate account", "account", aid, "same_user", ch.userID)
			ch.store.deleteAccount(aid)
		}
	}
}

// defaultStateDir returns the default weixin state directory.
func defaultStateDir() string {
	return config.WeixinStateDir()
}

// readStdinLine reads a line of text from stdin with a prompt.
// Used by the QR verify_code flow (need_verifycode).
func readStdinLine(prompt string) (string, error) {
	fmt.Print(prompt)
	var input string
	_, err := fmt.Scanln(&input)
	if err != nil {
		return "", err
	}
	return input, nil
}

// SystemPromptSuffix implements channel.SystemPromptSuffixer.
// Tells the agent it's currently operating as a personal WeChat bot
// connected via the iLink protocol, with platform-specific markdown limitations.
func (ch *Channel) SystemPromptSuffix() string {
	return `## Current Channel: WeChat (微信)

You are currently operating as a personal WeChat bot, connected via the iLink
protocol. Your responses are delivered through the WeChat messaging platform.

Platform characteristics:
- Limited markdown support: bold (**), italic (*), inline code (` + "`), code" + `
  fences (` + "```), tables, lists (-/*/1.), blockquotes (>), headings H1-H4, " + `
  horizontal rules (---, ***, ___)
- No HTML
- No images in markdown (![alt](url) is stripped)
- Headings H5/H6 are demoted to plain text
- Media files (images, documents, voice, video) are sent as separate
  attachments, not inline
- Synchronous request-reply model: one user message → one agent response
  per turn
- Keep responses concise and well-structured for mobile reading`
}

var _ channel.SystemPromptSuffixer = (*Channel)(nil)
