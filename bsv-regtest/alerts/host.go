package alerts

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	asp2p "github.com/bsv-blockchain/go-alert-system/app/p2p"
	"github.com/bsv-blockchain/go-sdk/util"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	ma "github.com/multiformats/go-multiaddr"
)

// DefaultProtocolID is the alert-system sync protocol teranode speaks.
const DefaultProtocolID = "/bitcoin/alert-system/1.0.0"

// Host is a bare libp2p host that speaks the alert-system sync protocol directly, without
// the library's DHT/gossip machinery. It gives deterministic, per-peer primitives:
//
//   - Probe: ask a peer for its latest alert sequence (IWantLatest -> IGotLatest);
//   - Push: make a peer pull alerts up to a sequence (IGotLatest{N} -> the peer requests
//     IWantSequenceNumber(latest+1..N), served from the Log);
//   - inbound syncs started by peers are answered from the Log, capped per peer by a
//     watermark, so a peer that discovers this host never learns more than intended.
type Host struct {
	h     host.Host
	proto protocol.ID
	log   *Log
	lg    *slog.Logger

	mu         sync.RWMutex
	watermarks map[peer.ID]uint32
	defaultWM  uint32
	timeout    time.Duration
}

// HostConfig configures NewHost.
type HostConfig struct {
	PrivateKeyHex string   // raw 64-byte Ed25519 key, hex; empty = ephemeral identity
	ListenAddrs   []string // multiaddrs, e.g. /ip4/0.0.0.0/tcp/9909; empty = no listener
	ProtocolID    string   // default DefaultProtocolID
	Timeout       time.Duration
	Logger        *slog.Logger
}

// NewHost starts the host. The Log is what inbound syncs and Push serve from.
func NewHost(cfg HostConfig, log *Log) (*Host, error) {
	if cfg.ProtocolID == "" {
		cfg.ProtocolID = DefaultProtocolID
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	opts := []libp2p.Option{}
	if cfg.PrivateKeyHex != "" {
		raw, err := hex.DecodeString(cfg.PrivateKeyHex)
		if err != nil {
			return nil, fmt.Errorf("decode private key: %w", err)
		}
		priv, err := crypto.UnmarshalEd25519PrivateKey(raw)
		if err != nil {
			return nil, fmt.Errorf("unmarshal private key: %w", err)
		}
		opts = append(opts, libp2p.Identity(priv))
	}
	if len(cfg.ListenAddrs) > 0 {
		opts = append(opts, libp2p.ListenAddrStrings(cfg.ListenAddrs...))
	} else {
		opts = append(opts, libp2p.NoListenAddrs)
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("libp2p host: %w", err)
	}
	hh := &Host{
		h:          h,
		proto:      protocol.ID(cfg.ProtocolID),
		log:        log,
		lg:         cfg.Logger,
		watermarks: map[peer.ID]uint32{},
		timeout:    cfg.Timeout,
	}
	h.SetStreamHandler(hh.proto, hh.serve)
	return hh, nil
}

// Close shuts the host down.
func (hh *Host) Close() error { return hh.h.Close() }

// ID returns the host's peer id.
func (hh *Host) ID() peer.ID { return hh.h.ID() }

// Addrs returns the host's full listen multiaddrs including /p2p/<id>.
func (hh *Host) Addrs() []string {
	var out []string
	for _, a := range hh.h.Addrs() {
		out = append(out, fmt.Sprintf("%s/p2p/%s", a, hh.h.ID()))
	}
	return out
}

// SetDefaultWatermark sets the sequence served to peers without an explicit watermark.
func (hh *Host) SetDefaultWatermark(seq uint32) {
	hh.mu.Lock()
	defer hh.mu.Unlock()
	hh.defaultWM = seq
}

// SetWatermark caps what an inbound sync from peer may learn.
func (hh *Host) SetWatermark(p peer.ID, seq uint32) {
	hh.mu.Lock()
	defer hh.mu.Unlock()
	hh.watermarks[p] = seq
}

func (hh *Host) watermark(p peer.ID) uint32 {
	hh.mu.RLock()
	defer hh.mu.RUnlock()
	if w, ok := hh.watermarks[p]; ok {
		return w
	}
	return hh.defaultWM
}

// Connect dials a peer given as a full multiaddr (…/p2p/<id>) and returns its id.
func (hh *Host) Connect(ctx context.Context, addr string) (peer.ID, error) {
	m, err := ma.NewMultiaddr(addr)
	if err != nil {
		return "", fmt.Errorf("multiaddr %q: %w", addr, err)
	}
	info, err := peer.AddrInfoFromP2pAddr(m)
	if err != nil {
		return "", fmt.Errorf("multiaddr %q: %w", addr, err)
	}
	ctx, cancel := context.WithTimeout(ctx, hh.timeout)
	defer cancel()
	if err := hh.h.Connect(ctx, *info); err != nil {
		return "", fmt.Errorf("connect %s: %w", addr, err)
	}
	return info.ID, nil
}

// deadline is the host timeout, or the caller's context deadline when that is sooner.
func (hh *Host) deadline(ctx context.Context) time.Time {
	dl := time.Now().Add(hh.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl = d
	}
	return dl
}

// Probe returns the peer's latest alert sequence. A failed probe closes the connection to the
// peer so the next one dials afresh: a connection that has gone dead (the peer was cut from the
// network) would otherwise be reused and keep the peer looking reachable.
func (hh *Host) Probe(ctx context.Context, addr string) (seq uint32, err error) {
	pid, err := hh.Connect(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = hh.h.Network().ClosePeer(pid)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, hh.timeout)
	defer cancel()
	s, err := hh.h.NewStream(ctx, pid, hh.proto)
	if err != nil {
		return 0, fmt.Errorf("open stream to %s: %w", pid, err)
	}
	defer s.Close()
	_ = s.SetDeadline(hh.deadline(ctx))
	if err := writeFrame(s, &asp2p.SyncMessage{Type: asp2p.IWantLatest}); err != nil {
		return 0, err
	}
	msg, err := readFrame(s)
	if err != nil {
		return 0, fmt.Errorf("read IGotLatest from %s: %w", pid, err)
	}
	if msg.Type != asp2p.IGotLatest {
		return 0, fmt.Errorf("unexpected sync message type 0x%02x from %s", msg.Type, pid)
	}
	return msg.SequenceNumber, nil
}

// PushResult reports what a Push delivered.
type PushResult struct {
	PeerLatestBefore uint32   `json:"peerLatestBefore"` // what the peer reported it had before (0 if it did not say)
	Delivered        []uint32 `json:"delivered"`        // sequences the peer requested and received
}

// Push tells the peer our latest is upTo and serves whatever it then requests. The peer
// (go-alert-system StreamThread) compares upTo with its own latest and pulls
// latest+1..upTo one at a time, verifying, executing and saving each. Returns once the
// peer closes the stream or the timeout elapses.
func (hh *Host) Push(ctx context.Context, addr string, upTo uint32) (*PushResult, error) {
	wire, ok := hh.log.Wire(upTo)
	if !ok {
		return nil, fmt.Errorf("alert %d is not in the log", upTo)
	}
	pid, err := hh.Connect(ctx, addr)
	if err != nil {
		return nil, err
	}
	hh.SetWatermark(pid, upTo)
	ctx, cancel := context.WithTimeout(ctx, hh.timeout)
	defer cancel()
	s, err := hh.h.NewStream(ctx, pid, hh.proto)
	if err != nil {
		return nil, fmt.Errorf("open stream to %s: %w", pid, err)
	}
	defer s.Close()
	_ = s.SetDeadline(hh.deadline(ctx))

	if err := writeFrame(s, &asp2p.SyncMessage{Type: asp2p.IGotLatest, SequenceNumber: upTo, Data: wire}); err != nil {
		return nil, err
	}
	res := &PushResult{}
	for {
		msg, err := readFrame(s)
		if err != nil {
			if errors.Is(err, io.EOF) || isClosed(err) {
				return res, nil
			}
			return res, fmt.Errorf("read from %s: %w", pid, err)
		}
		switch msg.Type {
		case asp2p.IWantSequenceNumber:
			w, ok := hh.log.Wire(msg.SequenceNumber)
			if !ok || msg.SequenceNumber > upTo {
				hh.lg.Warn("peer requested sequence we will not serve", "peer", pid, "seq", msg.SequenceNumber, "upTo", upTo)
				return res, nil
			}
			if err := writeFrame(s, &asp2p.SyncMessage{Type: asp2p.IGotSequenceNumber, SequenceNumber: msg.SequenceNumber, Data: w}); err != nil {
				return res, err
			}
			res.Delivered = append(res.Delivered, msg.SequenceNumber)
			if msg.SequenceNumber == upTo {
				// The peer closes after processing the last one; give it a moment so the
				// close is orderly, then return whether or not we observe it.
				_ = s.SetReadDeadline(time.Now().Add(2 * time.Second))
			}
		case asp2p.IGotLatest:
			// Peer answered our IGotLatest with its own view (some versions do); record it.
			res.PeerLatestBefore = msg.SequenceNumber
		default:
			hh.lg.Warn("unexpected sync message", "peer", pid, "type", msg.Type)
		}
	}
}

// serve answers inbound syncs from the Log, capped by the peer's watermark.
func (hh *Host) serve(s network.Stream) {
	defer s.Close()
	pid := s.Conn().RemotePeer()
	_ = s.SetDeadline(time.Now().Add(hh.timeout))
	wm := hh.watermark(pid)
	for {
		msg, err := readFrame(s)
		if err != nil {
			return
		}
		switch msg.Type {
		case asp2p.IWantLatest:
			wire, _ := hh.log.Wire(wm) // empty for 0 (genesis is local to every node)
			if err := writeFrame(s, &asp2p.SyncMessage{Type: asp2p.IGotLatest, SequenceNumber: wm, Data: wire}); err != nil {
				return
			}
			hh.lg.Debug("answered IWantLatest", "peer", pid, "seq", wm)
		case asp2p.IWantSequenceNumber:
			wire, ok := hh.log.Wire(msg.SequenceNumber)
			if !ok || msg.SequenceNumber > wm {
				hh.lg.Debug("refusing sequence above watermark", "peer", pid, "seq", msg.SequenceNumber, "watermark", wm)
				return
			}
			if err := writeFrame(s, &asp2p.SyncMessage{Type: asp2p.IGotSequenceNumber, SequenceNumber: msg.SequenceNumber, Data: wire}); err != nil {
				return
			}
			hh.lg.Info("served alert to peer", "peer", pid, "seq", msg.SequenceNumber)
		case asp2p.IGotLatest, asp2p.IGotSequenceNumber:
			// Peers push to us only if they think we are behind; we are the source, ignore.
			return
		default:
			return
		}
	}
}

// Frames are VarInt(len) || body, body = type(1) || seq u32 LE || data, exactly as
// go-alert-system's StreamThread writes and reads them.
func writeFrame(w io.Writer, msg *asp2p.SyncMessage) error {
	wr := util.NewWriter()
	wr.WriteIntBytes(msg.Serialize())
	_, err := w.Write(wr.Buf)
	return err
}

func readFrame(r io.Reader) (*asp2p.SyncMessage, error) {
	var vi util.VarInt
	if _, err := vi.ReadFrom(r); err != nil {
		return nil, err
	}
	if vi == 0 {
		return nil, io.EOF
	}
	if vi > 16<<20 {
		return nil, fmt.Errorf("sync frame too large: %d", vi)
	}
	b := make([]byte, vi)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return asp2p.NewSyncMessageFromBytes(b)
}

func isClosed(err error) bool {
	return errors.Is(err, network.ErrReset) || errors.Is(err, io.ErrUnexpectedEOF)
}
