package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/notify"
	"github.com/m0n5ter/m0nit0r/internal/store"
)

// Sender is the destination an alert goes to, which is *notify.Telegram in
// every build and a recorder in the tests.
type Sender interface {
	Send(ctx context.Context, text string) error
}

// alertInterval is how often the reachability of every peer is reconsidered.
// The thresholds it is measured against are minutes and hours, so this only
// needs to be fine enough not to add visible lag to them.
const alertInterval = 15 * time.Second

// Alerts announces peers going unreachable and coming back.
//
// What it reports is what this node itself observed - the same readings the
// dashboard draws its own column of the matrix from. A peer's opinion of a
// third node is not evidence about this node's route to it, so it is not what
// gets alerted on.
//
// Several nodes in a mesh can run this at once without sending duplicates. Each
// records what it announced in AlertNotices, those records reach the others in
// the ordinary sync, and before sending anything a node checks whether somebody
// has already said it. Stagger separates the decisions in time so that check
// has something to find: the node ranked first acts on schedule, the second one
// a stagger later, by which point the first one's notice has arrived - or has
// not, because the first node is itself the one that went down, and then the
// second one speaks up.
type Alerts struct {
	Store      *store.Store
	Telegram   Sender
	ServerID   int64
	ServerUID  string
	ServerName string
	DownAfter  time.Duration
	Repeat     time.Duration
	Stagger    time.Duration
	Log        *slog.Logger
}

// Run watches until ctx is cancelled.
func (w *Alerts) Run(ctx context.Context) {
	w.Log.Info("telegram alerts started",
		"downAfter", w.DownAfter, "repeat", w.Repeat, "stagger", w.Stagger)

	ticker := time.NewTicker(alertInterval)
	defer ticker.Stop()

	for {
		if err := w.round(ctx); err != nil {
			w.Log.Error("evaluate alerts", "err", err)
		}

		select {
		case <-ctx.Done():
			w.Log.Info("telegram alerts stopped")
			return
		case <-ticker.C:
		}
	}
}

func (w *Alerts) round(ctx context.Context) error {
	servers, err := w.Store.ListServers()
	if err != nil {
		return err
	}
	delay := w.rankDelay(servers)

	for _, srv := range servers {
		// Self is not something this node can observe a route to, and a peer
		// with no address is one that was removed or that only ever arrived as
		// a third-party mention: nothing probes it, so its last observation
		// stays whatever it was and would otherwise be alerted on for ever.
		// A push-only peer has no address either, but it is watched: the sync
		// worker records whether its pushes keep arriving.
		if srv.IsSelf || (srv.URL == "" && !srv.PushOnly) {
			continue
		}
		if err := w.consider(ctx, srv, delay); err != nil {
			w.Log.Error("evaluate alert", "peer", srv.Name, "err", err)
		}
	}
	return nil
}

// rankDelay is how long this node waits beyond the configured thresholds before
// it will speak. Alerting nodes are ordered by the identity they publish, which
// every node in the mesh agrees on and which nothing rotates, so each of them
// computes the same order and takes a different place in it.
func (w *Alerts) rankDelay(servers []model.Server) time.Duration {
	uids := make([]string, 0, len(servers))
	for _, srv := range servers {
		if srv.Alerts {
			uids = append(uids, srv.UID)
		}
	}
	sort.Strings(uids)

	for i, uid := range uids {
		if uid == w.ServerUID {
			return time.Duration(i) * w.Stagger
		}
	}
	// This node's own row always carries the flag while this worker is running,
	// so reaching here means the register was read mid-write. Acting first is
	// the safe reading of that: a duplicate message beats a silent outage.
	return 0
}

// consider decides whether one peer's current state is worth announcing, and
// announces it.
func (w *Alerts) consider(ctx context.Context, srv model.Server, delay time.Duration) error {
	available, since, found, err := w.Store.Streak(w.ServerID, srv.ID)
	if err != nil {
		return err
	}
	if !found {
		// Never probed - a peer added seconds ago, or a node this one has only
		// been told about. There is no state to have changed.
		return nil
	}

	last, announced, err := w.Store.LatestNotice(srv.ID)
	if err != nil {
		return err
	}

	now := time.Now()
	elapsed := now.Sub(since.Time)

	down, send := w.decide(available, since, last, announced, now, delay)
	if !send {
		return nil
	}

	if err := w.Telegram.Send(ctx, w.message(srv, down, since, elapsed)); err != nil {
		// Left unrecorded, so the next tick tries again rather than treating an
		// undelivered message as one the rest of the mesh should defer to.
		return err
	}

	w.Log.Info("alert sent", "peer", srv.Name, "down", down, "for", elapsed.Round(time.Second))

	return w.Store.InsertNotices(w.ServerID, []store.Notice{{
		ToServerID: srv.ID,
		Timestamp:  model.Now(),
		IsDown:     down,
	}})
}

// decide is the whole alerting rule for one peer, given the state of the route
// to it and the last thing anybody in the mesh announced about it. delay is
// this node's place in the stagger; every threshold is measured against it, so
// a node further down the order is always the later one to act - on the first
// message about an outage and on each hourly repeat alike.
//
// It reads no clocks and touches no storage, because every interesting case
// here is a matter of minutes or hours apart.
func (w *Alerts) decide(available bool, since model.Time, last store.Notice, announced bool,
	now time.Time, delay time.Duration) (down, send bool) {

	elapsed := now.Sub(since.Time)

	if !available {
		if elapsed < w.DownAfter+delay {
			return false, false
		}
		// Either nobody has reported this outage, or the last report of it is
		// old enough to be worth repeating. The repeat window carries the same
		// stagger as the first message, or every alerting node would come due
		// for the repeat at the same instant and none would have heard from
		// the others.
		if announced && last.IsDown && now.Sub(last.Timestamp.Time) < w.Repeat+delay {
			return false, false
		}
		return true, true
	}

	// Recovery is only news if a failure was announced, and only this node's
	// business if this node saw the failure: the run of successful checks has
	// to have begun after the report, not before it. Without that, a node that
	// could always reach the peer would announce a recovery from an outage that
	// only ever existed on somebody else's route - and it is the alert saying
	// the problem is over, so it is the one worth being strict about.
	if !announced || !last.IsDown || !since.After(last.Timestamp.Time) {
		return false, false
	}
	if elapsed < delay {
		return false, false
	}
	return false, true
}

// message renders one alert. Telegram's HTML is a short whitelist of inline
// tags, so everything interpolated goes through Escape.
func (w *Alerts) message(srv model.Server, down bool, since model.Time, elapsed time.Duration) string {
	var b strings.Builder

	name := notify.Escape(srv.Name)
	if down {
		fmt.Fprintf(&b, "🔴 <b>%s</b> is unreachable\n", name)
	} else {
		fmt.Fprintf(&b, "🟢 <b>%s</b> is reachable again\n", name)
	}

	if srv.Location != "" {
		fmt.Fprintf(&b, "Location: %s\n", notify.Escape(srv.Location))
	}

	if down {
		fmt.Fprintf(&b, "Down for: %s\n", humanDuration(elapsed))
		fmt.Fprintf(&b, "Unreachable since: %s\n", since.UTC().Format("2006-01-02 15:04:05 UTC"))
	} else {
		fmt.Fprintf(&b, "Back up for: %s\n", humanDuration(elapsed))
		fmt.Fprintf(&b, "Reachable since: %s\n", since.UTC().Format("2006-01-02 15:04:05 UTC"))
	}

	// Which node is reporting is not a detail: several may be configured to,
	// and an outage that only one of them can see is a different problem from
	// one they all agree on.
	fmt.Fprintf(&b, "Seen from: %s", notify.Escape(w.ServerName))
	return b.String()
}

// humanDuration renders a span the way a person reading an alert on a phone
// would: the two largest units that carry information, and no more. "1h 14m"
// rather than "1h14m3.482s".
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}
