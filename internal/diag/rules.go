package diag

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/teranode"
)

// Input is everything Evaluate is allowed to look at. Keeping it a plain struct makes the
// rule table a table test rather than something that needs a live stack.
type Input struct {
	Node  string
	Diag  *Diagnostics
	Fleet FleetContext
	// Prev is the previous sample, used only to tell a catchup that is progressing from
	// one that is wedged.
	Prev *Diagnostics
}

// ok reports whether a source succeeded.
func (in Input) ok(name string) bool {
	for _, s := range in.Diag.Sources {
		if s.Name == name {
			return s.OK
		}
	}
	return false
}

func (in Input) failed() []string {
	var out []string
	for _, s := range in.Diag.Sources {
		if !s.OK {
			out = append(out, s.Name)
		}
	}
	return out
}

func (in Input) sourceErr(name string) string {
	for _, s := range in.Diag.Sources {
		if s.Name == name {
			return s.Error
		}
	}
	return ""
}

// Evaluate reduces a node's evidence to one explanation. Rules are ordered so that a cause
// always outranks its own symptoms: a partition outranks the lag it produces, a failed
// catchup outranks "behind", and a fork outranks "behind" at equal height.
//
// The safety property: ClassInSync requires that every source succeeded. If any probe
// failed we fall through to ClassUnknown rather than inferring health from a height that
// happens to match — reporting healthy from missing data is the one outcome that would
// make this page worse than useless.
func Evaluate(in Input) Verdict {
	d := in.Diag
	f := in.Fleet
	node := in.Node

	behind := 0
	if d.Tip != nil && f.MaxHeight > d.Tip.Height {
		behind = int(f.MaxHeight - d.Tip.Height)
	}
	brief := f.Nodes[node]

	// 0. Not answering at all.
	if !in.ok(SrcTip) && !brief.Reachable {
		v := Verdict{
			Class: ClassUnreachable, Severity: "error",
			Headline: fmt.Sprintf("%s is not answering its asset API.", node),
			Evidence: []string{SrcTip},
		}
		if e := in.sourceErr(SrcTip); e != "" {
			v.Details = append(v.Details, e)
		}
		if d.ContainerState != "" && d.ContainerState != "running" {
			v.Details = append(v.Details, "its container is "+d.ContainerState)
		}
		return finish(v, in, SrcTip)
	}

	// 1. The container is not running: nothing else can be meaningful.
	if st := d.ContainerState; st != "" && st != "running" {
		v := Verdict{
			Class: ClassContainerDown, Severity: "error",
			Headline: fmt.Sprintf("%s's container is %s.", node, st),
			Evidence: []string{SrcContainer},
		}
		switch st {
		case "paused":
			v.Suggest = []string{"unpause the container from the Fleet or Chaos page"}
		case "exited", "dead", "created":
			v.Suggest = []string{"start the container from the Fleet or Chaos page"}
		}
		return finish(v, in, SrcContainer)
	}

	// 2. A partition the harness itself opened is the cause, not a symptom.
	if len(brief.Partitions) > 0 {
		v := Verdict{
			Class: ClassPartitioned, Severity: "warn",
			Headline: fmt.Sprintf("%s is cut off from the %s plane.", node, strings.Join(brief.Partitions, " and ")),
			Suggest:  []string{"heal the plane from the Fleet or Chaos page"},
			LogHint:  &LogHint{Services: "p2p,bval", Level: "INFO"},
		}
		if behind > 0 {
			v.Details = append(v.Details, fmt.Sprintf("it is %d block(s) behind the fleet (%d vs %d)", behind, tipHeight(d), f.MaxHeight))
		}
		return finish(v, in)
	}

	// 3. IDLE. Only the FSM endpoint reveals this: /health returns 200 in every state.
	if d.FSM != nil && d.FSM.State == "IDLE" {
		v := Verdict{
			Class: ClassFSMIdle, Severity: "error",
			Headline: fmt.Sprintf("%s is IDLE — it is not validating blocks or syncing.", node),
			Evidence: []string{SrcFSM},
			LogHint:  &LogHint{Services: "blkcC,bval", Level: "DEBUG", Grep: "FSM"},
		}
		if len(d.FSM.LegalEvents) > 0 {
			v.Suggest = []string{"legal transitions from here: " + strings.Join(d.FSM.LegalEvents, ", ")}
		}
		return finish(v, in, SrcFSM)
	}

	catching := (d.FSM != nil && d.FSM.State == "CATCHINGBLOCKS") || (d.Catchup != nil && d.Catchup.IsCatchingUp)

	// 4. Catching up but wedged. Checked before the reassuring "catching up" so a stall is
	// never hidden behind a progress bar.
	if catching && d.Catchup != nil && d.Catchup.DurationMs > 120_000 && stalled(d, in.Prev) {
		v := Verdict{
			Class: ClassCatchupStalled, Severity: "error",
			Headline: fmt.Sprintf("%s has been catching up for %s with no validated progress.",
				node, dur(d.Catchup.DurationMs)),
			Details:  catchupDetails(d, f),
			Evidence: []string{SrcCatchup},
			LogHint:  &LogHint{Services: "bval,p2p", Level: "INFO", Grep: "SyncCoordinator|catchup"},
		}
		return finish(v, in, SrcCatchup)
	}

	// 5. Catching up normally.
	if catching {
		c := d.Catchup
		head := fmt.Sprintf("%s is catching up", node)
		if c != nil && c.PeerNode != "" {
			head += " from " + c.PeerNode
		}
		if c != nil && c.TotalBlocks > 0 {
			head += fmt.Sprintf(": %d of %d blocks validated", c.BlocksValidated, c.TotalBlocks)
		}
		v := Verdict{
			Class: ClassCatchingUp, Severity: "info", Headline: head + ".",
			Details:  catchupDetails(d, f),
			Evidence: []string{SrcFSM, SrcCatchup},
			LogHint:  &LogHint{Services: "bval", Level: "INFO", Grep: "catchup"},
		}
		return finish(v, in, SrcFSM)
	}

	// 6. Not catching up, and the last attempt failed recently. The failure is the
	// explanation; "behind" would merely restate the observation.
	if d.Catchup != nil && d.Catchup.PreviousAttempt != nil {
		pa := d.Catchup.PreviousAttempt
		if pa.ErrorMessage != "" && recent(pa.AttemptTime, 10*time.Minute) {
			from := pa.PeerNode
			if from == "" {
				from = shortID(pa.PeerID)
			}
			v := Verdict{
				Class: ClassCatchupFailed, Severity: "error",
				Headline: fmt.Sprintf("%s's last catchup from %s failed (%s).", node, from, orUnknown(pa.ErrorType)),
				Details:  []string{pa.ErrorMessage},
				Evidence: []string{SrcCatchup},
				LogHint:  &LogHint{Services: "bval,stval", Level: "WARN"},
			}
			if behind > 0 {
				v.Details = append(v.Details, fmt.Sprintf("it is %d block(s) behind and is not retrying", behind))
			}
			switch pa.ErrorType {
			case "validation_failure":
				v.Suggest = []string{"check this node's rejected blocks below — it refused what the peer served"}
			case "network_error":
				v.Suggest = []string{"check the p2p plane for a partition"}
			case "secret_mining":
				v.Suggest = []string{"the peer served a chain it had not announced"}
			}
			return finish(v, in, SrcCatchup)
		}
	}

	// 7. No peers at all: a connectivity problem, not a validation one.
	if in.ok(SrcPeers) && countConnected(d.Peers) == 0 {
		return finish(Verdict{
			Class: ClassNoPeers, Severity: "error",
			Headline: fmt.Sprintf("%s has no connected P2P peers.", node),
			Evidence: []string{SrcPeers},
			LogHint:  &LogHint{Services: "p2p", Level: "INFO"},
		}, in, SrcPeers)
	}

	// 8. At the fleet's height but on another chain: a fork, not lag.
	if d.Tip != nil && f.MajorityTip != "" && d.Tip.Hash != f.MajorityTip &&
		d.Tip.Height+1 >= f.MaxHeight && len(d.Invalid) > 0 {
		if rel := relevantRejections(d, f); len(rel) > 0 {
			v := Verdict{
				Class: ClassMinorityTip, Severity: "error",
				Headline: fmt.Sprintf("%s is on a minority tip: it rejected %s.", node, describeRejections(rel)),
				Evidence: []string{SrcTip, SrcInvalid},
				LogHint:  &LogHint{Services: "bval,stval", Level: "WARN"},
			}
			for _, b := range rel {
				if b.RejectRootCause != "" {
					v.Details = append(v.Details, b.RejectRootCause)
				}
			}
			return finish(v, in, SrcTip, SrcInvalid)
		}
	}

	// 9. Simply behind.
	if d.Tip != nil && behind > 1 {
		v := Verdict{
			Class: ClassBehind, Severity: "warn",
			Headline: fmt.Sprintf("%s is %d blocks behind the fleet (%d vs %d) and is not catching up.",
				node, behind, d.Tip.Height, f.MaxHeight),
			Evidence: []string{SrcTip},
			LogHint:  &LogHint{Services: "bval,p2p", Level: "INFO"},
		}
		if ahead := peersAhead(d.Peers, d.Tip.Height); ahead > 0 {
			v.Details = append(v.Details, fmt.Sprintf("it can see %d peer(s) ahead of it but has not started a catchup", ahead))
		}
		if e := worstPeerError(d.Peers); e != "" {
			v.Details = append(v.Details, "last peer error: "+e)
		}
		return finish(v, in, SrcTip)
	}

	// 10. In sync but a dependency is unhappy.
	if d.Health != nil && len(d.Health.Unhealthy) > 0 {
		return finish(Verdict{
			Class: ClassUnhealthy, Severity: "warn",
			Headline: fmt.Sprintf("%s is in sync but %s reporting a failure.", node, plural(d.Health.Unhealthy)),
			Details:  unhealthyDetails(d.Health),
			Evidence: []string{SrcHealth},
		}, in, SrcHealth)
	}

	// 11. Healthy — but only when nothing at all failed. See the safety property above.
	if in.ok(SrcTip) && in.ok(SrcFSM) && d.Tip != nil && d.FSM != nil &&
		d.FSM.State == "RUNNING" && (f.MajorityTip == "" || d.Tip.Hash == f.MajorityTip) &&
		len(in.failed()) == 0 {
		return finish(Verdict{
			Class: ClassInSync, Severity: "ok",
			Headline: fmt.Sprintf("%s is RUNNING and in sync at height %d.", node, d.Tip.Height),
			Evidence: []string{SrcTip, SrcFSM},
		}, in, SrcTip, SrcFSM)
	}

	// 12. Not enough evidence to say.
	v := Verdict{
		Class: ClassUnknown, Severity: "unknown",
		Headline: fmt.Sprintf("Cannot explain %s's state: %s did not answer.", node, strings.Join(in.failed(), ", ")),
	}
	if len(in.failed()) == 0 {
		v.Headline = fmt.Sprintf("Cannot classify %s's state from the evidence available.", node)
	}
	return finish(v, in)
}

// finish records which sources the matched rule relied on and downgrades confidence when
// any of them were missing.
func finish(v Verdict, in Input, want ...string) Verdict {
	v.Confidence = "high"
	if len(v.Evidence) == 0 && len(want) > 0 {
		v.Evidence = want
	}
	for _, w := range want {
		if !in.ok(w) {
			v.Missing = append(v.Missing, w)
		}
	}
	for _, s := range in.Diag.Sources {
		if !s.OK && !containsStr(v.Missing, s.Name) && v.Class == ClassUnknown {
			v.Missing = append(v.Missing, s.Name)
		}
	}
	if len(v.Missing) > 0 {
		v.Confidence = "low"
	}
	return v
}

func stalled(cur, prev *Diagnostics) bool {
	if prev == nil || prev.Catchup == nil || cur.Catchup == nil {
		return false
	}
	return prev.Catchup.BlocksValidated == cur.Catchup.BlocksValidated
}

func catchupDetails(d *Diagnostics, f FleetContext) []string {
	c := d.Catchup
	if c == nil {
		return nil
	}
	var out []string
	if c.TargetBlockHeight > 0 {
		out = append(out, fmt.Sprintf("target height %d (fleet is at %d)", c.TargetBlockHeight, f.MaxHeight))
	}
	if c.BlocksFetched != c.BlocksValidated {
		out = append(out, fmt.Sprintf("%d fetched but only %d validated", c.BlocksFetched, c.BlocksValidated))
	}
	if c.ForkDepth > 1 {
		out = append(out, fmt.Sprintf("fork depth %d — this is a reorg, not simple lag", c.ForkDepth))
	}
	if c.CommonAncestorHeigh > 0 {
		out = append(out, fmt.Sprintf("chains diverge at height %d", c.CommonAncestorHeigh))
	}
	return out
}

// relevantRejections keeps rejections that could explain the CURRENT divergence: blocks at
// or above this node's tip. Older rejections are history and must not drive the verdict.
func relevantRejections(d *Diagnostics, f FleetContext) []teranode.InvalidBlock {
	var out []teranode.InvalidBlock
	for _, b := range d.Invalid {
		if d.Tip != nil && b.Height > d.Tip.Height && b.Height <= f.MaxHeight+1 {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Height < out[j].Height })
	return out
}

func describeRejections(bs []teranode.InvalidBlock) string {
	miners := map[string]bool{}
	for _, b := range bs {
		if n := b.MinerNode; n != "" {
			miners[n] = true
		}
	}
	who := make([]string, 0, len(miners))
	for m := range miners {
		who = append(who, m)
	}
	sort.Strings(who)
	s := fmt.Sprintf("%d block(s)", len(bs))
	if len(who) > 0 {
		s += " mined by " + strings.Join(who, ", ")
	}
	if len(bs) == 1 {
		return s + fmt.Sprintf(" at height %d", bs[0].Height)
	}
	return s + fmt.Sprintf(" at heights %d-%d", bs[0].Height, bs[len(bs)-1].Height)
}

func unhealthyDetails(h *teranode.Health) []string {
	var out []string
	for _, d := range h.Deps {
		if d.OK {
			continue
		}
		s := fmt.Sprintf("%s: HTTP %d", d.Resource, d.Status)
		if d.Error != "" {
			s += " — " + d.Error
		} else if d.Message != "" {
			s += " — " + d.Message
		}
		out = append(out, s)
	}
	return out
}

func countConnected(ps []teranode.Peer) int {
	n := 0
	for _, p := range ps {
		if p.IsConnected {
			n++
		}
	}
	return n
}

func peersAhead(ps []teranode.Peer, h uint32) int {
	n := 0
	for _, p := range ps {
		if p.IsConnected && p.Height > h {
			n++
		}
	}
	return n
}

func worstPeerError(ps []teranode.Peer) string {
	for _, p := range ps {
		if p.LastCatchupError != "" {
			return p.LastCatchupError
		}
	}
	return ""
}

func tipHeight(d *Diagnostics) uint32 {
	if d.Tip == nil {
		return 0
	}
	return d.Tip.Height
}

func recent(unixSecs int64, within time.Duration) bool {
	if unixSecs <= 0 {
		return false
	}
	return time.Since(time.Unix(unixSecs, 0)) <= within
}

func dur(ms int64) string { return (time.Duration(ms) * time.Millisecond).Round(time.Second).String() }

func orUnknown(s string) string {
	if s == "" {
		return "no error type given"
	}
	return s
}

func shortID(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	if s == "" {
		return "an unknown peer"
	}
	return s
}

func plural(names []string) string {
	if len(names) == 1 {
		return names[0] + " is"
	}
	return fmt.Sprintf("%d dependencies are", len(names))
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
