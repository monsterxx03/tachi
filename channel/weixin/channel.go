package weixin

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/monsterxx03/tachi/channel"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// Channel implements the channel.Channel interface for WeChat iLink Bot.
type Channel struct {
	cfg            config.WeixinConfig
	store          *stateStore
	cli            *client
	typingTickets  *typingTicketCache

	// Resolved at login time.
	accountID string   // ilink_bot_id, e.g. "a1b2c3d4@im.bot"
	userID    string   // ilink_user_id, the scanner's WeChat ID
	botToken  string   // bearer token for API calls

	logger *debuglog.Logger
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
		logger:        debuglog.DefaultLogger.WithSource("channel:weixin"),
	}, nil
}

// Name returns the channel type identifier.
func (ch *Channel) Name() string { return "weixin" }

// Run starts the channel's message loop. It first attempts to load stored
// credentials; if none exist, it performs a QR-code login flow. Then it
// enters the long-polling receive loop.
func (ch *Channel) Run(ctx context.Context, handler channel.MessageHandler) error {
	ch.cli.SetRouteTag(ch.cfg.RouteTag)

	// --- Load existing account or login ---
	if err := ch.loadOrLogin(ctx); err != nil {
		if ctx.Err() != nil {
			return nil // cancelled during login
		}
		return fmt.Errorf("weixin: login: %w", err)
	}

	ch.cli.SetBotToken(ch.botToken)

	ch.logger.Log("weixin: logged in as %s (bot=%s, user=%s)", ch.accountID, ch.botToken[:8]+"...", ch.userID)
	fmt.Printf("[weixin] logged in as %s\n", ch.accountID)

	// --- Long-polling loop ---
	return ch.pollingLoop(ctx, handler)
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
			ch.logger.Log("weixin: auto-adding user %s to allowlist", ch.userID)
			allowFrom := &AllowFromData{
				Version:   1,
				AllowFrom: []string{ch.userID},
			}
			ch.store.saveAllowFrom(ch.accountID, allowFrom)
		}

		ch.logger.Log("weixin: loaded stored account %s", accounts[0])
		return nil
	}

	// No stored account → start QR login.
	return ch.qrLogin(ctx)
}

// qrLogin performs the QR-code login flow: fetch QR → poll status until
// confirmed or cancelled.
func (ch *Channel) qrLogin(ctx context.Context) error {
	fmt.Println("[weixin] no stored account found. starting QR login...")
	fmt.Println("[weixin] open the following URL and scan with WeChat:")

	qr, err := ch.cli.getBotQRCode()
	if err != nil {
		return fmt.Errorf("get QR code: %w", err)
	}

	fmt.Printf("\n  %s\n\n", qr.QRCodeImgContent)
	fmt.Println("[weixin] waiting for scan... (timeout: 240s)")

	const overallTimeout = 240 * time.Second
	deadline := time.Now().Add(overallTimeout)
	refreshCount := 0
	const maxQRRefreshes = 3

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		status, err := ch.cli.getQRCodeStatus(qr.QRCode)
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
				ch.logger.Log("weixin: warning: failed to save allowFrom: %v", err)
			}

			// Clean up duplicate accounts for the same userId.
			ch.deduplicateAccounts()
			return nil

		case QRStatusWait:
			// Continue polling.

		case QRStatusScaned:
			fmt.Println("[weixin] scanned! waiting for confirmation on phone...")

		case QRStatusExpired:
			if refreshCount >= maxQRRefreshes {
				return fmt.Errorf("QR code expired after %d refreshes", maxQRRefreshes)
			}
			fmt.Println("[weixin] QR expired, refreshing...")
			qr, err = ch.cli.getBotQRCode()
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
			ch.logger.Log("weixin: removing duplicate account %s (same user %s)", aid, ch.userID)
			ch.store.deleteAccount(aid)
		}
	}
}

// defaultStateDir returns the default weixin state directory under ~/.tachi.
func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".tachi/weixin"
	}
	return home + "/.tachi/weixin"
}
