package node

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/zeroconf/v2"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// What this machine announces, and what a reader gets out of it.
//
// The announcement is the other half of node/discovery.go, so the two are tested against each other
// in shape: the fields this side writes are the fields that side reads, and the names it makes are
// names that side can key.

// The instance name is what a person sees in a discovery browser, so it carries the name this
// machine calls itself AND the front of its identity — because the name is not a fact (every
// machine's default is the same word, and anyone may announce any name at all).
func TestTheInstanceNameCarriesTheNameAndTheIdentity(t *testing.T) {
	require.Equal(t, "tachi@home-nas-"+nasID[:8], instanceName("tachi@home-nas", nasID),
		"the names this project actually uses survive whole")
	require.Equal(t, "家里的机器-"+nasID[:8], instanceName("家里的机器", nasID),
		"and a machine named in Chinese is a machine named in Chinese")

	// The two characters a DNS-SD instance name cannot hold become dashes: the library joins the parts
	// with dots and does NOT escape what is in them, so a dot here would cut the name in half.
	require.Equal(t, "home-nas-"+nasID[:8], instanceName("home.nas", nasID))
	require.Equal(t, "home-nas-"+nasID[:8], instanceName("home\\nas", nasID))
	require.Equal(t, "home-nas-"+nasID[:8], instanceName("home\nnas", nasID))
	require.Equal(t, "home-nas-"+nasID[:8], instanceName("home nas", nasID),
		"whitespace is a separator a person typed, so it reads as one")
	require.Equal(t, "home--nas-"+nasID[:8], instanceName("home  nas", nasID))

	// A name that says nothing still produces a usable label: the identity is the half that matters.
	require.Equal(t, "tachi-"+nasID[:8], instanceName("", nasID))
	require.Equal(t, "tachi-"+nasID[:8], instanceName("   ", nasID))

	// The configured half is bounded, so a long name cannot push the identity off the end of what a
	// browser shows.
	long := instanceName(strings.Repeat("a", 200), nasID)
	require.Equal(t, strings.Repeat("a", instanceNameMaxRunes)+"-"+nasID[:8], long)
	require.True(t, strings.HasSuffix(long, nasID[:8]), "the identity is the part that must survive")
}

// The TXT records are three facts and no more: what this node says about itself in a packet anyone on
// the network can read. The name, the description and the skill list are deliberately absent — those
// live in the Agent Card, which a reader only gets after connecting and proving who is answering.
func TestTheAnnouncementSaysThreeFactsAndNothingElse(t *testing.T) {
	txt := advertiseTXT(nasID)
	require.Equal(t, []string{"id=" + nasID, "v=1", "scheme=https"}, txt)

	// And they are exactly the keys the browser reads back, spelled the same way: the two halves are
	// one arrangement, and a rename of a key on one side would silently stop discovery on the other.
	announced := announcement{TXT: map[string]string{}}
	for _, kv := range txt {
		key, value, ok := strings.Cut(kv, "=")
		require.True(t, ok, "%q is not a key=value TXT record", kv)
		announced.TXT[key] = value
	}
	// A browser of a DIFFERENT machine, because this machine's own announcement is one it must ignore.
	other, err := newMDNSBrowser("ffffffffffffffffffffffffffffffff", config.MdnsDiscoveryConfig{}, nil)
	require.NoError(t, err)
	cand, why := other.decide(announcement{
		Instance: "some-machine",
		TXT:      announced.TXT,
		Addrs:    []string{"https://192.168.1.9:7080"},
		Seen:     time.Now(),
	})
	require.Empty(t, why, "the browser has to accept what the advertiser writes")
	require.NotNil(t, cand)
	require.Equal(t, nasID, cand.Identity)
}

// A node that cannot announce has to say why rather than announce something untrue: no identity, an
// address with no port, and an interface this machine does not have are all refusals.
func TestAnnouncingRefusesWhatItCannotDo(t *testing.T) {
	id, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)
	o := Options{Name: "tachi", Logger: logger.New("node-test")}

	_, err = startAdvertising(o, "0.0.0.0:7080", nil)
	require.ErrorContains(t, err, "no identity")

	// Unresolvable in the sense that matters: the caller passes what the surface BOUND, and something
	// that is not an address cannot carry the port the announcement has to name.
	_, err = startAdvertising(o, "not-an-address", id)
	require.ErrorContains(t, err, "is not an address")

	_, err = startAdvertising(o, "0.0.0.0:0", id)
	require.ErrorContains(t, err, "no port")

	// A named interface that is not on this machine refuses the start: falling back to every interface
	// would announce this machine onto networks the setting exists to keep it off.
	o.AdvertiseInterfaces = []string{"definitely-not-an-interface-here"}
	_, err = startAdvertising(o, "0.0.0.0:7080", id)
	require.ErrorContains(t, err, "not an interface")
}

// skipNoMulticast is what this test says when the machine it runs on cannot multicast: the failure it
// would otherwise report is about the environment, not about this code.
const skipNoMulticast = "no announcement came back: multicast is not usable here (a container, a " +
	"firewall, or a network with no multicast-capable interface)"

// The real thing, on real multicast: what this machine announces is what a browser on the same
// network can see. It is a SMOKE test in the design's terms (阶段 2), and it skips when the machine
// running it has no usable multicast — the failure it would otherwise report is about the
// environment, not about this code.
func TestAnAnnouncementIsVisibleOnTheNetwork(t *testing.T) {
	id, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)

	// A port nobody is listening on: this test is about the records, and the advertiser deliberately
	// does not check that the port answers (that is the listener's business, and the reader proves it
	// by connecting).
	adv, err := startAdvertising(Options{Name: "tachi-smoke", Logger: logger.New("node-test")},
		"0.0.0.0:7080", id)
	if err != nil {
		t.Skipf("this machine cannot announce (no usable multicast?): %v", err)
	}
	t.Cleanup(adv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	entries := make(chan *zeroconf.ServiceEntry)
	go func() { _ = zeroconf.Browse(ctx, mdnsServiceType, mdnsDomain, entries) }()

	// The channel is the LIBRARY's to close (it does, when the browse ends), so nothing here closes it
	// a second time — and the deadline is what ends the loop when no announcement arrives.
	for {
		select {
		case entry, ok := <-entries:
			if !ok {
				t.Skip(skipNoMulticast)
			}
			if entry.Instance != adv.instance {
				continue
			}
			require.Equal(t, 7080, entry.Port)
			require.Contains(t, entry.Text, "id="+id.NodeID)
			require.Contains(t, entry.Text, "v=1")
			require.Contains(t, entry.Text, "scheme=https")
			return
		case <-ctx.Done():
			t.Skip(skipNoMulticast)
		}
	}
}
