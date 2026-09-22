package diag

import (
	"context"
	"strings"

	"github.com/bsv-blockchain/bsv-regtest/topology"
	"github.com/bsv-blockchain/chaos-test/internal/chaos"
	"github.com/bsv-blockchain/chaos-test/internal/logs"
)

// annotateRejections fills in WHY a block was rejected.
//
// The /blocks/invalid endpoint lists what a node refused but carries no reason; the reason
// only exists in the container log. One bounded read per node (not per block) is taken and
// then searched in memory, so the cost does not scale with the number of rejections.
func (s *Service) annotateRejections(ctx context.Context, n topology.Node, d *Diagnostics) {
	if len(d.Invalid) == 0 {
		return
	}
	res, err := s.d.Runtime.Logs(ctx, n.Container, chaos.LogOptions{
		Tail: 4000, Timestamps: true, MaxBytes: 4 << 20,
	})
	if err != nil {
		return // the reason is a bonus; its absence must not degrade the report
	}
	lines, _ := logs.Scan(res.Text, logs.ScanOptions{})

	for i := range d.Invalid {
		hash := d.Invalid[i].Hash
		if hash == "" {
			continue
		}
		// teranode abbreviates hashes in log prefixes, so match on a prefix too.
		short := hash
		if len(short) > 12 {
			short = short[:12]
		}
		for j := len(lines) - 1; j >= 0; j-- {
			l := &lines[j]
			if logs.Rank(l.Level) < logs.Rank("WARN") {
				continue
			}
			hay := l.Msg + strings.Join(l.Cont, "\n")
			if !strings.Contains(hay, hash) && !strings.Contains(hay, short) {
				continue
			}
			d.Invalid[i].RejectReason = l.Msg
			d.Invalid[i].RejectRootCause = l.RootCause
			d.Invalid[i].RejectCode = l.Code
			break
		}
	}
}
