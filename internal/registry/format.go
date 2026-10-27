package registry

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
)

// FormatWho renders `leadkit who`'s human table: one row per session (name · host · role
// · leads · project · age/liveness), plus a prominent AMBIGUOUS-LEADERSHIP banner when
// 2+ live leads claim the same slug. live is the TTL-filtered set; stale is non-nil only
// under --all (rendered greyed as "(stale)"). note carries the degrade marker (e.g.
// "(possibly stale — sync failed)") when the pull failed.
func FormatWho(live, stale []Entry, note string, now time.Time) string {
	var b strings.Builder
	if note != "" {
		fmt.Fprintf(&b, "%s\n", note)
	}
	if len(live) == 0 && len(stale) == 0 {
		b.WriteString("no sessions registered (within the liveness window)\n")
		return b.String()
	}

	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tHOST\tROLE\tLEADS\tPROJECT\tAGE\tLIVE")
	for _, e := range live {
		writeRow(tw, e, now, "live")
	}
	for _, e := range stale {
		writeRow(tw, e, now, "stale")
	}
	tw.Flush()

	if amb := AmbiguousLeadership(live); len(amb) > 0 {
		b.WriteString("\n!! AMBIGUOUS-LEADERSHIP — 2+ live leads claim the same workstream:\n")
		for _, c := range amb {
			var who []string
			for _, e := range c.Leaders {
				who = append(who, fmt.Sprintf("%s@%s", e.DisplayName(), e.Host))
			}
			fmt.Fprintf(&b, "   %s: %s — resolve who leads it (a two-lead coordination lock is the follow-up)\n",
				c.Slug, strings.Join(who, ", "))
		}
	}
	return b.String()
}

func writeRow(tw *tabwriter.Writer, e Entry, now time.Time, liveness string) {
	leads := strings.Join(e.Leads, ",")
	if leads == "" {
		leads = "-"
	}
	project := e.Project
	if project == "" {
		project = "-"
	}
	name := e.DisplayName()
	age := now.Sub(e.UpdatedAt).Round(time.Second)
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, e.Host, e.Role, leads, project, age, liveness)
}
