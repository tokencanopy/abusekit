package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
)

func TestGoldenEventsAndTimers(t *testing.T) {
	cfg, brands, webmail := loadShippedRuleConfig(t)
	input := `{"id":"created","subject":"acct_test","type":"subject.created","at":"2031-01-01T00:00:00Z","data":{}}
{"id":"key","subject":"acct_test","type":"resource.created","at":"2031-01-01T00:01:00Z","data":{"kind":"key"}}
`
	var out bytes.Buffer
	err := WriteGolden(context.Background(), &out, "example", strings.NewReader(input), cfg, brands, webmail)
	if err != nil {
		t.Fatal(err)
	}
	var rows []GoldenRow
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var row GoldenRow
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	if len(rows) != 5 {
		t.Fatalf("want 2 event rows and 3 timer rows, got %d", len(rows))
	}
	if rows[0].Trigger != "event" || rows[1].Trigger != "event" || rows[2].At != "2031-01-01T01:05:00Z" {
		t.Fatalf("unexpected timeline: %+v", rows)
	}
	if rows[len(rows)-1].NextRescoreAt != "" {
		t.Fatal("must drain all timers")
	}
	if rows[1].Features["core.resource_total"] != "3ff0000000000000" {
		t.Fatal("must store float64 bits, not rounded decimals")
	}
}

func TestGoldenReference(t *testing.T) {
	if want := os.Getenv("ABUSEKIT_GOLDEN_EXPECT_PROFILE"); want != "" && GoldenProfile() != want {
		t.Fatalf("CI requires numeric profile %s, got %s", want, GoldenProfile())
	}
	cfg, brands, webmail := loadShippedRuleConfig(t)
	var out bytes.Buffer
	if err := WriteGoldenFixtures(context.Background(), &out, filepath.Join(repoRoot(t), "eval", "fixtures"), cfg, brands, webmail); err != nil {
		t.Fatal(err)
	}
	name := "reference-ns.jsonl"
	if profile := GoldenProfile(); profile != "amd64-fma" {
		name = "reference-ns-" + profile + ".jsonl"
	}
	want, err := os.ReadFile(filepath.Join(repoRoot(t), "eval", "golden", name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatal("golden differs: scoring migration needs an explicit baseline review")
	}
}

func TestGoldenCanonicalTiesAndNoFutureNeighbors(t *testing.T) {
	cfg, brands, webmail := loadShippedRuleConfig(t)
	lines := []string{
		`{"producer":"z","id":"p","subject":"acct_a","type":"payment.attempt","at":"2031-01-01T00:00:00Z","data":{"outcome":"succeeded","funding":"prepaid"}}`,
		`{"producer":"a","id":"p","subject":"acct_a","type":"payment.attempt","at":"2031-01-01T00:00:00Z","data":{"outcome":"succeeded","funding":"credit"},"links":{"card_fingerprint_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
		`{"producer":"b","id":"p","subject":"acct_b","type":"subject.created","at":"2031-01-01T00:00:00Z","data":{},"links":{"card_fingerprint_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
	}
	var a, b bytes.Buffer
	if err := WriteGolden(context.Background(), &a, "ties", strings.NewReader(strings.Join(lines, "\n")), cfg, brands, webmail); err != nil {
		t.Fatal(err)
	}
	lines[0], lines[2] = lines[2], lines[0]
	if err := WriteGolden(context.Background(), &b, "ties", strings.NewReader(strings.Join(lines, "\n")), cfg, brands, webmail); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("input order changed canonical replay")
	}
	rows := bytes.Split(a.Bytes(), []byte("\n"))
	var first, third GoldenRow
	json.Unmarshal(rows[0], &first)
	json.Unmarshal(rows[2], &third)
	if first.Features["core.fingerprint_seen_on_other_subjects"] != "0000000000000000" {
		t.Fatal("future tied neighbor leaked")
	}
	if third.Features["core.first_funding_prepaid"] != "0000000000000000" {
		t.Fatal("first funding must resolve by producer before id")
	}
	if third.Features["core.fingerprint_seen_on_other_subjects"] != "3ff0000000000000" {
		t.Fatal("accepted tied neighbor missing")
	}
}

func TestGoldenRejectsMalformedDuplicateAndCancellation(t *testing.T) {
	cfg, brands, webmail := loadShippedRuleConfig(t)
	row := `{"id":"x","subject":"acct_x","type":"subject.created","at":"2031-01-01T00:00:00Z","data":{}}`
	for _, input := range []string{"", `{"unexpected":true}`, row + "\n" + row} {
		if err := WriteGolden(context.Background(), &bytes.Buffer{}, "bad", strings.NewReader(input), cfg, brands, webmail); err == nil {
			t.Fatalf("accepted invalid input %q", input)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WriteGolden(ctx, &bytes.Buffer{}, "cancel", strings.NewReader(row), cfg, brands, webmail); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestGoldenPreservesLabelIdentity(t *testing.T) {
	cfg, brands, webmail := loadShippedRuleConfig(t)
	input := `{"producer":"operator","id":"original-label","subject":"acct_label","type":"label","at":"2031-01-01T00:00:00Z","data":{"label":"abusive"}}`
	var out bytes.Buffer
	if err := WriteGolden(context.Background(), &out, "label", strings.NewReader(input), cfg, brands, webmail); err != nil {
		t.Fatal(err)
	}
	var row GoldenRow
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &row); err != nil {
		t.Fatal(err)
	}
	if row.EventID != "original-label" || row.Producer != "operator" {
		t.Fatalf("label identity changed: %+v", row)
	}
}

func TestGoldenRejectsTrailingJSON(t *testing.T) {
	cfg, brands, webmail := loadShippedRuleConfig(t)
	row := `{"id":"x","subject":"acct_x","type":"subject.created","at":"2031-01-01T00:00:00Z","data":{}}`
	if err := WriteGolden(context.Background(), &bytes.Buffer{}, "trailing", strings.NewReader(row+" "+row), cfg, brands, webmail); err == nil {
		t.Fatal("silently discarded the second JSON value")
	}
}

func TestNeighborsCapUsesCanonicalKeys(t *testing.T) {
	at := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	histories := map[string][]event.Event{}
	for group := 0; group < 5; group++ {
		hash := fmt.Sprintf("%064x", group+1)
		histories["target"] = append(histories["target"], event.Event{At: at, Links: event.Links{DeviceHash: hash}})
		for member := 0; member < 50; member++ {
			subject := fmt.Sprintf("group%d_member%02d", group, member)
			histories[subject] = []event.Event{{At: at, Links: event.Links{DeviceHash: hash}}}
		}
	}
	n := newDatasetNeighbors(histories, nil)
	for attempt := 0; attempt < 30; attempt++ {
		subjects, truncated := n.neighborsByKinds("target", []string{"device_hash"}, at.Add(time.Second))
		if !truncated || len(subjects) != 200 {
			t.Fatalf("unexpected cap: %d, %v", len(subjects), truncated)
		}
		for _, subject := range subjects {
			if strings.HasPrefix(subject, "group4_") {
				t.Fatal("cap selected a later key before an earlier one")
			}
		}
	}
}
