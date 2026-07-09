package transport

import (
	"context"
	"log"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	circuitclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
)

// ReserveRelays makes an explicit circuit-relay v2 reservation on each relay so
// this host is reachable through it, then keeps each reservation refreshed
// before expiry until ctx is done. It returns the peer IDs of the relays whose
// first reservation succeeded (so the caller knows the synthesized relayed
// address is dialable). Explicit reservation is used instead of AutoRelay
// because AutoRelay only reserves once AutoNAT concludes the host is private,
// which is slow and inconclusive on many networks.
func ReserveRelays(ctx context.Context, h host.Host, relayAddrs []string, logger *log.Logger) []peer.ID {
	var ok []peer.ID
	for _, ai := range parseAddrInfos(relayAddrs) {
		ai := ai
		res, err := reserveOnce(ctx, h, ai)
		if err != nil {
			logger.Printf("relay reservation on %s failed: %v", ai.ID, err)
			continue
		}
		logger.Printf("relay reservation on %s ok (expires %s)", ai.ID, res.Expiration.Format(time.RFC3339))
		ok = append(ok, ai.ID)
		go refreshLoop(ctx, h, ai, res.Expiration, logger)
	}
	return ok
}

// reserveOnce connects to the relay (if needed) and reserves a slot.
func reserveOnce(ctx context.Context, h host.Host, ai peer.AddrInfo) (*circuitclient.Reservation, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if len(ai.Addrs) > 0 {
		if err := h.Connect(cctx, ai); err != nil {
			return nil, err
		}
	}
	return circuitclient.Reserve(cctx, h, ai)
}

// refreshLoop re-reserves before the current reservation expires until ctx is
// done. On failure it retries with a short backoff.
func refreshLoop(ctx context.Context, h host.Host, ai peer.AddrInfo, expiry time.Time, logger *log.Logger) {
	for {
		// Refresh a few minutes before expiry (never sleep less than a minute).
		wait := time.Until(expiry) - 5*time.Minute
		if wait < time.Minute {
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		res, err := reserveOnce(ctx, h, ai)
		if err != nil {
			logger.Printf("relay reservation refresh on %s failed: %v (retrying)", ai.ID, err)
			expiry = time.Now().Add(2 * time.Minute) // short retry cadence
			continue
		}
		expiry = res.Expiration
	}
}
