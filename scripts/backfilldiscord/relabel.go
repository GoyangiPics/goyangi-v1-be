package main

import (
	"fmt"
	"log"
	"strings"

	"goyangi-v1-be/hooks"
)

// -relabel FROM:TO rewrites the origin of every content and set that carries
// FROM. The point is the "script" marker: a backfill runs under it so it can be
// looked at as a whole, then this turns it into plain "discord" once trusted.
// The PATCH goes through the same provenance hook a create does, so it needs
// the same server and is refused for anything a superuser may not claim.
func runRelabel(baseURL, email, password, spec string, commit bool) {
	from, to, ok := strings.Cut(spec, ":")
	if !ok || from == "" || to == "" {
		log.Fatal("-relabel wants FROM:TO, e.g. script:discord")
	}
	if to != hooks.OriginDiscord && to != hooks.OriginScript {
		log.Fatalf("-relabel: %q is not an origin a superuser may claim", to)
	}
	if email == "" || password == "" {
		log.Fatal("superuser credentials required: PB_ADMIN_EMAIL and PB_ADMIN_PASSWORD")
	}

	pb := newPBClient(baseURL)
	if err := pb.authenticate(email, password); err != nil {
		log.Fatalf("PocketBase auth failed: %v", err)
	}
	log.Printf("🔑 PocketBase: %s", pb.baseURL)
	if !commit {
		log.Printf("🔍 DRY RUN — nothing will be written. Pass -commit to apply.")
	}

	filter := fmt.Sprintf("origin = %s", pbQuote(from))
	for _, collection := range []string{"contents", "contents_sets"} {
		records, err := pb.listAll(collection, filter, "id")
		if err != nil {
			log.Fatalf("%s: %v", collection, err)
		}
		log.Printf("📋 %s: %d record(s) with origin %q", collection, len(records), from)
		if !commit {
			continue
		}
		done := 0
		for _, rec := range records {
			if _, err := pb.update(collection, rec.str("id"), map[string]any{"origin": to}); err != nil {
				log.Printf("   ❌ %s: %v", rec.str("id"), err)
				continue
			}
			done++
		}
		log.Printf("   ✅ %d/%d relabelled %s → %s", done, len(records), from, to)
	}
}
