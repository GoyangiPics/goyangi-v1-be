package bot

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
// order, read-only.
func dryRunVerdict(in inboundMessage, plan ingestPlan) ingestResult {
	if isClaimed(in.msg.ID.String()) {
		return ingestResult{status: statusDuplicate, reason: reasonAlreadyHandled}
	}
	if alreadyIngested(in.jumpLink()) {
		return ingestResult{status: statusDuplicate, reason: reasonAlreadyArchived}
	}

	m := plan.metadata
	idolIDs, groupIDs, missing := resolveSubjects(m)
	preview := &ingestPreview{
		Trigger:    plan.trigger,
		Uploader:   m.Uploader,
		Title:      m.Title,
		Idols:      namesByIDs("groups_idols", idolIDs),
		Groups:     namesByIDs("groups", groupIDs),
		Unresolved: missing,
		Tags:       splitTrim(m.Tags),
	}
	if plan.joinsSet && m.SetId != "" {
		preview.JoinsSet = publicBaseURL + "/set/" + m.SetId
	}
	for _, item := range plan.items {
		preview.Items = append(preview.Items, previewItem{URL: item.URL, Filename: item.Filename, Filetype: item.Filetype})
	}
	result := ingestResult{status: statusIngested, preview: preview}

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
