package bot

import "strings"

// Dry runs: what ingesting a message would do, without doing it.
//
// For whoever builds a bot that forwards messages over the ingest API: they can
// point it at real traffic and check every verdict — which rule fired, who gets
// credited, which idols resolve, which items would be fetched — before a single
// post lands on the site. Nothing is claimed, saved or logged, and no follow-up
// slot is spent. Read-only all the way down: idols and groups resolve against
// the directory (resolveSubjects), and uploaders are looked up rather than
// created.

// ingestPreview is what a dry run reports.
type ingestPreview struct {
	// role_ping, repeat_ping, mention, reply, follow_up or text.
	Trigger  string `json:"trigger"`
	Uploader string `json:"uploader"`
	Title    string `json:"title,omitempty"`
	// What resolved, by the names on the site.
	Idols  []string `json:"idols"`
	Groups []string `json:"groups"`
	// Names that matched nothing: "idol X", "group Y".
	Unresolved []string      `json:"unresolved,omitempty"`
	Tags       []string      `json:"tags,omitempty"`
	Items      []previewItem `json:"items"`
	// The existing set a reply or follow-up would join.
	JoinsSet string `json:"joins_set,omitempty"`
}

// previewItem is one item a run would fetch.
type previewItem struct {
	URL      string `json:"url"`
	Filename string `json:"filename,omitempty"`
	Filetype string `json:"filetype,omitempty"`
}

// dryRunVerdict is ingestInbound's answer for a dry run, from the point where
// the plan is settled: the same checks the real run makes next, in the same
// order, read-only — and the preview whatever the verdict.
func dryRunVerdict(in inboundMessage, plan ingestPlan) ingestResult {
	m := plan.metadata
	idolIDs, groupIDs, missing := resolveSubjects(m)
	preview := &ingestPreview{
		Trigger:    plan.trigger,
		Uploader:   m.Uploader,
		Title:      previewTitle(m, idolIDs, groupIDs),
		Idols:      namesByIDs("groups_idols", idolIDs),
		Groups:     namesByIDs("groups", groupIDs),
		Unresolved: missing,
		Tags:       splitTrim(m.Tags),
		Items:      previewItems(plan.items, m.Filetype),
	}
	if plan.joinsSet && m.SetId != "" {
		preview.JoinsSet = publicBaseURL + "/set/" + m.SetId
	}
	result := ingestResult{status: statusIngested, preview: preview}
	result.outcome.total = len(plan.items)

	// Already taken — by goyangi's own bot, typically, while it still runs in
	// the server. The preview stays: it is what the other bot compares.
	switch {
	case isClaimed(in.msg.ID.String()):
		result.status, result.reason = statusDuplicate, reasonAlreadyHandled
		return result
	case alreadyIngested(in.jumpLink()):
		result.status, result.reason = statusDuplicate, reasonAlreadyArchived
		return result
	}

	// runIngestion's order: any credited uploader blocked, then any opted out,
	// then idols and groups.
	var blocked, optedOut bool
	for _, name := range splitTrim(m.Uploader) {
		rec, err := findUploaderByNames(name)
		if err != nil || rec == nil {
			continue // a new uploader: neither blocked nor opted out
		}
		blocked = blocked || rec.GetBool("blockIngest")
		optedOut = optedOut || rec.GetBool("skipDiscordImport")
	}
	switch {
	case blocked:
		result.status, result.reason = statusSkipped, reasonBlocked
	case optedOut:
		result.status, result.reason = statusSkipped, reasonOptedOut
	case len(idolIDs) == 0 || len(groupIDs) == 0:
		result.status, result.reason = statusFailed, reasonUnresolved
	}
	return result
}

// previewTitle is the title runIngestion would give: a generated title is
// rewritten from the names that resolved, an explicit `title:` is kept.
func previewTitle(m Metadata, idolIDs, groupIDs []string) string {
	if m.Title != autoTitle(m.Idol, m.Group) {
		return m.Title
	}
	resolved := autoTitle(strings.Join(namesByIDs("groups_idols", idolIDs), ", "), strings.Join(namesByIDs("groups", groupIDs), ", "))
	if resolved == "" {
		return m.Title
	}
	return resolved
}

// previewItems are the items as a run would type them: a `filetype:` line
// applies to every item.
func previewItems(items []MediaItem, filetype string) []previewItem {
	out := make([]previewItem, 0, len(items))
	for _, item := range items {
		ft := item.Filetype
		if filetype != "" {
			ft = filetype
		}
		out = append(out, previewItem{URL: item.URL, Filename: item.Filename, Filetype: ft})
	}
	return out
}
