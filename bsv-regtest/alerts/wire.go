// Package alerts builds, signs, stores and delivers BSV alert-system messages.
//
// Message construction reuses go-alert-system's models so the bytes are exactly what a
// teranode (which embeds the same library) will parse. Payload encoders follow the
// library's *parsers* (hack/publish.go's informational and confiscation builders omit
// the VarInt length prefix the parsers require).
package alerts

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/bsv-blockchain/go-alert-system/app/models"
	"github.com/bsv-blockchain/go-alert-system/app/models/model"
	"github.com/bsv-blockchain/go-alert-system/utils"
	"github.com/bsv-blockchain/go-sdk/util"
)

// Type is the alert type identifier carried in the message header.
type Type uint32

// Alert types as defined by go-alert-system (app/models/alert_types.go).
const (
	TypeInformational   Type = 0x01
	TypeFreezeUTXO      Type = 0x02
	TypeUnfreezeUTXO    Type = 0x03
	TypeConfiscateUTXO  Type = 0x04
	TypeBanPeer         Type = 0x05
	TypeUnbanPeer       Type = 0x06
	TypeInvalidateBlock Type = 0x07
	TypeSetKeys         Type = 0x08
)

// String returns the lower-case name of the type.
func (t Type) String() string {
	switch t {
	case TypeInformational:
		return "informational"
	case TypeFreezeUTXO:
		return "freeze"
	case TypeUnfreezeUTXO:
		return "unfreeze"
	case TypeConfiscateUTXO:
		return "confiscate"
	case TypeBanPeer:
		return "ban"
	case TypeUnbanPeer:
		return "unban"
	case TypeInvalidateBlock:
		return "invalidateblock"
	case TypeSetKeys:
		return "setkeys"
	default:
		return fmt.Sprintf("type-%d", uint32(t))
	}
}

// ParseType maps a type name (as printed by Type.String) back to the Type.
func ParseType(s string) (Type, error) {
	for _, t := range []Type{TypeInformational, TypeFreezeUTXO, TypeUnfreezeUTXO, TypeConfiscateUTXO,
		TypeBanPeer, TypeUnbanPeer, TypeInvalidateBlock, TypeSetKeys} {
		if t.String() == s {
			return t, nil
		}
	}
	return 0, fmt.Errorf("unknown alert type %q", s)
}

// Fund is one output in a freeze or unfreeze alert. Heights follow SV Node semantics:
// the consensus window is [EnforceAtHeightStart, EnforceAtHeightStop), Stop exclusive;
// Stop <= Start is an empty interval (the shape an unfreeze takes).
type Fund struct {
	TxID                       string `json:"txid"` // display (big-endian) hex
	Vout                       uint32 `json:"vout"`
	EnforceAtHeightStart       uint64 `json:"enforceAtHeightStart"`
	EnforceAtHeightStop        uint64 `json:"enforceAtHeightStop"`
	PolicyExpiresWithConsensus bool   `json:"policyExpiresWithConsensus"`
}

// FundsPayload encodes funds for a freeze or unfreeze alert: N x 57 bytes,
// txid[32] || vout u64 LE || start u64 LE || stop u64 LE || policyExpires u8.
// The txid bytes are hex-decoded verbatim: the receiving node re-encodes them with
// hex.EncodeToString into the RPC "txId", so display order round-trips.
func FundsPayload(funds []Fund) ([]byte, error) {
	if len(funds) == 0 {
		return nil, errors.New("freeze/unfreeze alert needs at least one fund")
	}
	var out []byte
	for i, f := range funds {
		raw, err := hex.DecodeString(f.TxID)
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("fund %d: txid must be 32 bytes of hex: %v", i, err)
		}
		mf := models.Fund{
			Vout:                       uint64(f.Vout),
			EnforceAtHeightStart:       f.EnforceAtHeightStart,
			EnforceAtHeightEnd:         f.EnforceAtHeightStop,
			PolicyExpiresWithConsensus: f.PolicyExpiresWithConsensus,
		}
		copy(mf.TransactionOutID[:], raw)
		out = append(out, mf.Serialize()...)
	}
	return out, nil
}

// InformationalPayload encodes a free-text alert: VarInt(len) || message.
func InformationalPayload(message string) []byte {
	w := util.NewWriter()
	w.WriteIntBytes([]byte(message))
	return w.Buf
}

// InvalidateBlockPayload encodes hash[32] || VarInt(len) || reason. The hash is given in
// display hex; the parser reads the 32 bytes with chainhash.NewHash, which expects the
// internal (reversed) byte order, so the display hex is reversed here.
func InvalidateBlockPayload(blockHashDisplayHex, reason string) ([]byte, error) {
	raw, err := hex.DecodeString(blockHashDisplayHex)
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("block hash must be 32 bytes of hex: %v", err)
	}
	if reason == "" {
		return nil, errors.New("invalidate block alert needs a non-empty reason")
	}
	w := util.NewWriter()
	w.WriteBytesReverse(raw)
	w.WriteIntBytes([]byte(reason))
	return w.Buf, nil
}

// ConfiscatePayload encodes enforceAtHeight u64 LE || VarInt(len) || rawTx.
func ConfiscatePayload(enforceAtHeight uint64, rawTx []byte) ([]byte, error) {
	if len(rawTx) == 0 {
		return nil, errors.New("confiscation alert needs a raw transaction")
	}
	w := util.NewWriter()
	w.WriteBytes(binary.LittleEndian.AppendUint64(nil, enforceAtHeight))
	w.WriteIntBytes(rawTx)
	return w.Buf, nil
}

// PeerPayload encodes VarInt(len) || peer || VarInt(len) || reason for ban/unban alerts.
// peer is what the node passes to setban, e.g. "192.168.1.2/24".
func PeerPayload(peer, reason string) ([]byte, error) {
	if peer == "" || reason == "" {
		return nil, errors.New("ban/unban alert needs a peer and a reason")
	}
	w := util.NewWriter()
	w.WriteIntBytes([]byte(peer))
	w.WriteIntBytes([]byte(reason))
	return w.Buf, nil
}

// SetKeysPayload encodes exactly five 33-byte compressed public keys.
func SetKeysPayload(compressedPubKeysHex []string) ([]byte, error) {
	if len(compressedPubKeysHex) != 5 {
		return nil, fmt.Errorf("set keys alert needs exactly 5 keys, got %d", len(compressedPubKeysHex))
	}
	var out []byte
	for i, k := range compressedPubKeysHex {
		raw, err := hex.DecodeString(k)
		if err != nil || len(raw) != 33 {
			return nil, fmt.Errorf("key %d: must be 33 bytes of hex: %v", i, err)
		}
		out = append(out, raw...)
	}
	return out, nil
}

// Alert is a built (and, once Sign has run, signed) alert message.
type Alert struct {
	Sequence  uint32    `json:"sequence"`
	Type      Type      `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Payload   []byte    `json:"payload"`
	// Unsigned is the header+payload that signatures cover.
	Unsigned []byte `json:"unsigned"`
	// Wire is the complete message as published: Unsigned followed by the signatures.
	Wire []byte `json:"wire"`
	// Hash is the double-SHA256 of Unsigned, as go-alert-system stores it.
	Hash string `json:"hash"`
}

// Version is the alert message version go-alert-system emits and expects.
const Version uint32 = 1

// Build assembles an alert and signs it with signingKeysHex (32-byte hex private keys).
// go-alert-system requires exactly three signatures; the receiving node checks each
// against its active genesis keys, so the three must be distinct genesis keys.
func Build(seq uint32, typ Type, payload []byte, ts time.Time, signingKeysHex []string) (*Alert, error) {
	if len(signingKeysHex) != 3 {
		return nil, fmt.Errorf("alerts need exactly 3 signing keys, got %d", len(signingKeysHex))
	}
	if len(payload) == 0 {
		return nil, errors.New("alert payload must not be empty")
	}
	m := models.NewAlertMessage(model.New())
	m.SetVersion(Version)
	m.SetTimestamp(uint64(ts.Unix()))
	m.SequenceNumber = seq
	m.SetAlertType(models.AlertType(typ))
	m.SetRawMessage(payload)
	m.SerializeData()

	sigs, err := utils.SignWithKeys(m.GetRawData(), signingKeysHex)
	if err != nil {
		return nil, fmt.Errorf("sign alert: %w", err)
	}
	m.SetSignatures(sigs)

	return &Alert{
		Sequence:  seq,
		Type:      typ,
		Timestamp: ts,
		Payload:   payload,
		Unsigned:  append([]byte(nil), m.GetRawData()...),
		Wire:      m.Serialize(),
		Hash:      m.Hash,
	}, nil
}

// Parse decodes wire bytes with go-alert-system's parser (header, payload and the three
// signatures). It does not verify signatures (that needs a datastore of active keys).
func Parse(wire []byte) (*models.AlertMessage, error) {
	m, err := models.NewAlertFromBytes(wire, model.New())
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Funds decodes the outputs of a freeze or unfreeze alert from its wire bytes (the inverse
// of FundsPayload). Other alert types yield nil, nil.
func Funds(wire []byte) ([]Fund, error) {
	m, err := Parse(wire)
	if err != nil {
		return nil, err
	}
	if t := Type(m.GetAlertType()); t != TypeFreezeUTXO && t != TypeUnfreezeUTXO {
		return nil, nil
	}
	raw := m.GetRawMessage()
	if len(raw) == 0 || len(raw)%57 != 0 {
		return nil, fmt.Errorf("funds payload is %d bytes, not a multiple of 57", len(raw))
	}
	out := make([]Fund, 0, len(raw)/57)
	for i := 0; i+57 <= len(raw); i += 57 {
		r := raw[i : i+57]
		out = append(out, Fund{
			TxID:                       hex.EncodeToString(r[:32]),
			Vout:                       uint32(binary.LittleEndian.Uint64(r[32:40])),
			EnforceAtHeightStart:       binary.LittleEndian.Uint64(r[40:48]),
			EnforceAtHeightStop:        binary.LittleEndian.Uint64(r[48:56]),
			PolicyExpiresWithConsensus: r[56] != 0,
		})
	}
	return out, nil
}

// Describe returns the library's human-readable rendering of an alert's payload, e.g.
// "Freezing UTXO [txid:vout] ...", by running the type-specific parser.
func Describe(wire []byte) (string, error) {
	m, err := Parse(wire)
	if err != nil {
		return "", err
	}
	am := m.ProcessAlertMessage()
	if am == nil {
		return "", fmt.Errorf("unknown alert type %d", m.GetAlertType())
	}
	if err := am.Read(m.GetRawMessage()); err != nil {
		return "", fmt.Errorf("parse %s payload: %w", Type(m.GetAlertType()), err)
	}
	return am.MessageString(), nil
}
