package facade

import (
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/metrics"
)

// 同步过期的预热条目只降为未同步、项目保留：补池时不再新建项目（以前过期就丢弃重建，
// 用不上的项目一直堆在账号里）。领取时优先同步完的。
func TestWarmPoolKeepsExpiredProjects(t *testing.T) {
	w := newWarmPool(2)
	old := time.Now().Add(-2 * warmEntryTTL)
	w.byAccount["a"] = []warmEntry{{projectID: "p1", synced: true, warmedAt: old}, {projectID: "p2", synced: true, warmedAt: time.Now()}}
	w.gcAccount("a", time.Now())
	es := w.byAccount["a"]
	if len(es) != 2 || es[0].synced || !es[1].synced {
		t.Fatalf("过期条目应保留并标为未同步: %+v", es)
	}

	r := &Runner{warm: w, app: metrics.NewApp()}
	if id, ok := r.warmTake("a"); !ok || id != "p2" {
		t.Fatalf("应优先领走同步完的: %q", id)
	}
	if id, ok := r.warmTake("a"); !ok || id != "p1" {
		t.Fatalf("同步过期的项目照样可领: %q", id)
	}
}
