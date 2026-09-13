package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func linkProposalDoc(t *testing.T, conn *sql.DB, proposalID, docID int) {
	t.Helper()
	if err := db.LinkProposalDoc(conn, proposalID, docID); err != nil {
		t.Fatalf("LinkProposalDoc(%d,%d): %v", proposalID, docID, err)
	}
}

// DKT-518: the human renderer padded every continuation line of a multi-line
// vote summary out to the width of the widest line, so a 15KB proposal rendered
// as 115KB. Human output must stay close to the JSON size and carry no
// gratuitously long lines.
func TestVoteShow_MultiLineSummaryIsNotPadded(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	conn := newTestDB(t)
	pid := createProposal(t, conn, "Wide summary", string(model.ProposalStatusOpen))

	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, "    list-smartcards   Display available smartcards.")
	}
	lines = append(lines, strings.Repeat("w", 1600))
	summary := strings.Join(lines, "\n")

	_, err := db.CastVote(conn, &model.Vote{
		ProposalID:      pid,
		VoterName:       "seat-a",
		VoterRole:       "reviewer",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Summary:         summary,
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	cmd := cmdWithDB(conn)
	wJSON, jsonBuf := bufWriter(true)
	err = runVoteShow(cmd, []string{model.FormatProposalID(pid)}, wJSON)
	testsupport.Must(t, err, "runVoteShow (json): %v", err)

	cmd = cmdWithDB(conn)
	wHuman, humanBuf := bufWriter(false)
	err = runVoteShow(cmd, []string{model.FormatProposalID(pid)}, wHuman)
	testsupport.Must(t, err, "runVoteShow (human): %v", err)

	human := humanBuf.String()
	for i, line := range strings.Split(human, "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line %d has trailing whitespace: %q", i, line)
		}
		if len(line) > 200 && !strings.Contains(line, strings.Repeat("w", 1600)) {
			t.Errorf("line %d is %d chars, expected short: %q", i, len(line), line)
		}
	}

	if max := int(float64(jsonBuf.Len()) * 1.2); humanBuf.Len() > max {
		t.Errorf("human output %d bytes, json %d bytes (max %d)",
			humanBuf.Len(), jsonBuf.Len(), max)
	}
}

func TestVoteShowJSON_LinkedDocsArrayShapeAndOrder(t *testing.T) {
	conn := newTestDB(t)
	pid := createProposal(t, conn, "Ratify the TDD", string(model.ProposalStatusOpen))
	docB := createDoc(t, conn, "Beta", "adr", "accepted")
	docA := createDoc(t, conn, "Alpha", "tdd", "approved")
	linkProposalDoc(t, conn, pid, docB)
	linkProposalDoc(t, conn, pid, docA)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	err := runVoteShow(cmd, []string{model.FormatProposalID(pid)}, w)
	testsupport.Must(t, err, "runVoteShow: %v", err)

	var env struct {
		Data struct {
			LinkedDocs []string `json:"linked_docs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	got := env.Data.LinkedDocs
	want := []string{"DOC-1", "DOC-2"}
	if len(got) != len(want) {
		t.Fatalf("len(linked_docs) = %d, want %d:\n%s", len(got), len(want), buf.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("linked_docs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestVoteShowJSON_LinkedDocsEmptyIsArray(t *testing.T) {
	conn := newTestDB(t)
	pid := createProposal(t, conn, "No docs", string(model.ProposalStatusOpen))

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	err := runVoteShow(cmd, []string{model.FormatProposalID(pid)}, w)
	testsupport.Must(t, err, "runVoteShow: %v", err)

	var raw map[string]json.RawMessage
	err = json.Unmarshal(buf.Bytes(), &raw)
	testsupport.Must(t, err, "unmarshal envelope: %v", err)
	var data map[string]json.RawMessage
	err = json.Unmarshal(raw["data"], &data)
	testsupport.Must(t, err, "unmarshal data: %v", err)
	docsRaw, ok := data["linked_docs"]
	if !ok {
		t.Fatalf("linked_docs key absent:\n%s", buf.String())
	}
	if string(docsRaw) != "[]" {
		t.Errorf("empty linked_docs = %s, want []", docsRaw)
	}
}

func TestVoteShow_RendersLinkedDocsSection(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	conn := newTestDB(t)
	pid := createProposal(t, conn, "Ratify the TDD", string(model.ProposalStatusOpen))
	doc := createDoc(t, conn, "Docket Doc CLI", "tdd", "approved")
	linkProposalDoc(t, conn, pid, doc)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(false)
	err := runVoteShow(cmd, []string{model.FormatProposalID(pid)}, w)
	testsupport.Must(t, err, "runVoteShow: %v", err)

	out := buf.String()
	if !strings.Contains(out, "Linked Docs") {
		t.Fatalf("styled output missing Linked Docs header:\n%s", out)
	}
	if !strings.Contains(out, "DOC-1") {
		t.Errorf("styled output missing DOC-1:\n%s", out)
	}
}

func TestVoteShow_OmitsLinkedDocsWhenEmpty(t *testing.T) {
	for _, tc := range []struct {
		name    string
		noColor bool
	}{
		{"styled", false},
		{"plain", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noColor {
				t.Setenv("NO_COLOR", "1")
			} else {
				t.Setenv("TERM", "xterm-256color")
			}
			conn := newTestDB(t)
			pid := createProposal(t, conn, "No docs", string(model.ProposalStatusOpen))

			cmd := cmdWithDB(conn)
			w, buf := bufWriter(false)
			err := runVoteShow(cmd, []string{model.FormatProposalID(pid)}, w)
			testsupport.Must(t, err, "runVoteShow: %v", err)
			if strings.Contains(buf.String(), "Linked Docs") {
				t.Errorf("empty proposal should omit Linked Docs section:\n%s", buf.String())
			}
		})
	}
}

// sealedProposal opens a proposal under the sealed rendering rule (DKT-2447)
// with room for two casts, so one cast leaves it open and the second closes
// it — the two states the sealed tests read across.
func sealedProposal(t *testing.T, conn *sql.DB) int {
	t.Helper()
	id, err := db.CreateProposal(conn, &model.Proposal{
		Description:    "Sealed ballot",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 2,
		Threshold:      0.5,
		Sealed:         true,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	return id
}

// castSealedSeat casts one fully-populated ballot: a verdict, both weights, a
// summary and structured findings — every field a sibling seat could anchor
// on, so a leak of any one of them is visible.
func castSealedSeat(t *testing.T, conn *sql.DB, pid int, voter string) {
	t.Helper()
	_, err := db.CastVote(conn, &model.Vote{
		ProposalID:      pid,
		VoterName:       voter,
		VoterRole:       "reviewer",
		Verdict:         model.VerdictApproveWithConcerns,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Summary:         "the summary of " + voter,
		FindingsJSON:    &model.Findings{Concerns: []string{"a concern from " + voter}},
	})
	testsupport.Must(t, err, "CastVote(%s): %v", voter, err)
}

// voteShowData runs `vote show --json` and returns the envelope's data object
// with its votes decoded generically, so a test can ask which KEYS are on the
// wire rather than which values a typed decode defaulted.
func voteShowData(t *testing.T, conn *sql.DB, pid int) (map[string]json.RawMessage, []map[string]any) {
	t.Helper()
	w, buf := bufWriter(true)
	err := runVoteShow(cmdWithDB(conn), []string{model.FormatProposalID(pid)}, w)
	testsupport.Must(t, err, "runVoteShow: %v", err)

	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	var votes []map[string]any
	if err := json.Unmarshal(env.Data["votes"], &votes); err != nil {
		t.Fatalf("unmarshal votes: %v\n%s", err, env.Data["votes"])
	}
	return env.Data, votes
}

// sealedCastFields are the vote keys a sealed-open proposal must NOT put on
// the wire: everything a sibling seat could read a verdict or its reasoning
// out of.
var sealedCastFields = []string{
	"verdict", "confidence", "domain_relevance", "effective_weight",
	"findings", "findings_json", "summary",
}

// DKT-2447: a proposal opened sealed withholds every cast's verdict, weights,
// findings and summary from `vote show --json` while it is open — a seat that
// reads the ballot after a sibling has cast sees only WHO has cast — and
// carries all of them once the tally closes it.
func TestVoteShowJSON_SealedProposalWithholdsCastsUntilFinalized(t *testing.T) {
	conn := newTestDB(t)
	pid := sealedProposal(t, conn)

	castSealedSeat(t, conn, pid, "seat-a")
	data, votes := voteShowData(t, conn, pid)

	if string(data["status"]) != `"open"` || string(data["sealed"]) != `true` {
		t.Fatalf("after one cast: status=%s sealed=%s, want open and true",
			data["status"], data["sealed"])
	}
	if len(votes) != 1 || votes[0]["voter_name"] != "seat-a" {
		t.Fatalf("open sealed proposal lists votes %v, want one entry naming seat-a", votes)
	}
	for _, key := range sealedCastFields {
		if _, present := votes[0][key]; present {
			t.Errorf("open sealed proposal carries %q on the wire: %s", key, data["votes"])
		}
	}
	for _, leaked := range []string{"the summary of seat-a", "a concern from seat-a",
		string(model.VerdictApproveWithConcerns)} {
		if strings.Contains(string(data["votes"]), leaked) {
			t.Errorf("open sealed proposal leaks %q: %s", leaked, data["votes"])
		}
	}

	castSealedSeat(t, conn, pid, "seat-b")
	data, votes = voteShowData(t, conn, pid)

	if string(data["status"]) != `"approved"` {
		t.Fatalf("after quorum: status=%s, want approved", data["status"])
	}
	if len(votes) != 2 {
		t.Fatalf("closed proposal lists %d votes, want 2: %s", len(votes), data["votes"])
	}
	for _, v := range votes {
		for _, key := range sealedCastFields {
			if _, present := v[key]; !present {
				t.Errorf("closed proposal omits %q for %v: %s", key, v["voter_name"], data["votes"])
			}
		}
		if v["verdict"] != string(model.VerdictApproveWithConcerns) {
			t.Errorf("closed proposal verdict for %v = %v, want %s",
				v["voter_name"], v["verdict"], model.VerdictApproveWithConcerns)
		}
		if v["summary"] != "the summary of "+v["voter_name"].(string) {
			t.Errorf("closed proposal summary for %v = %v", v["voter_name"], v["summary"])
		}
	}
}

// The human rendering of a sealed-open proposal names who has cast and how
// many casts are in, and nothing of what they cast — in both the styled and
// the plain renderer.
func TestVoteShow_SealedProposalRendersOnlyVoterNamesWhileOpen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		noColor bool
	}{
		{"styled", false},
		{"plain", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noColor {
				t.Setenv("NO_COLOR", "1")
			} else {
				t.Setenv("TERM", "xterm-256color")
			}
			conn := newTestDB(t)
			pid := sealedProposal(t, conn)
			castSealedSeat(t, conn, pid, "seat-a")

			w, buf := bufWriter(false)
			err := runVoteShow(cmdWithDB(conn), []string{model.FormatProposalID(pid)}, w)
			testsupport.Must(t, err, "runVoteShow: %v", err)
			out := buf.String()

			for _, want := range []string{"seat-a", "1/2", "sealed"} {
				if !strings.Contains(out, want) {
					t.Errorf("sealed-open rendering lacks %q:\n%s", want, out)
				}
			}
			for _, leaked := range []string{string(model.VerdictApproveWithConcerns),
				"the summary of seat-a", "a concern from seat-a", "conf=", "weight="} {
				if strings.Contains(out, leaked) {
					t.Errorf("sealed-open rendering leaks %q:\n%s", leaked, out)
				}
			}

			castSealedSeat(t, conn, pid, "seat-b")
			w, buf = bufWriter(false)
			err = runVoteShow(cmdWithDB(conn), []string{model.FormatProposalID(pid)}, w)
			testsupport.Must(t, err, "runVoteShow after quorum: %v", err)
			out = buf.String()
			for _, want := range []string{string(model.VerdictApproveWithConcerns),
				"the summary of seat-a", "a concern from seat-b"} {
				if !strings.Contains(out, want) {
					t.Errorf("closed rendering lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

// `vote result --json` follows the same rule as `vote show --json` (DKT-2447):
// while a sealed proposal is open its votes array names the casters and
// carries no verdict, weights, findings or summary; the count and quorum
// fields still answer "is the ballot still waiting".
func TestVoteResultJSON_SealedProposalWithholdsCastsUntilFinalized(t *testing.T) {
	conn := newTestDB(t)
	pid := sealedProposal(t, conn)
	castSealedSeat(t, conn, pid, "seat-a")

	result := func() (map[string]json.RawMessage, []map[string]any) {
		t.Helper()
		w, buf := bufWriter(true)
		err := runVoteResult(cmdWithDB(conn), []string{model.FormatProposalID(pid)}, w)
		testsupport.Must(t, err, "runVoteResult: %v", err)
		var env struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, buf.String())
		}
		var votes []map[string]any
		if err := json.Unmarshal(env.Data["votes"], &votes); err != nil {
			t.Fatalf("unmarshal votes: %v\n%s", err, env.Data["votes"])
		}
		return env.Data, votes
	}

	data, votes := result()
	if string(data["sealed"]) != `true` || string(data["votes_cast"]) != `1` ||
		string(data["quorum_reached"]) != `false` {
		t.Errorf("sealed-open result: sealed=%s votes_cast=%s quorum_reached=%s",
			data["sealed"], data["votes_cast"], data["quorum_reached"])
	}
	if len(votes) != 1 || votes[0]["voter_name"] != "seat-a" {
		t.Fatalf("sealed-open result votes = %v, want one entry naming seat-a", votes)
	}
	for _, key := range sealedCastFields {
		if _, present := votes[0][key]; present {
			t.Errorf("sealed-open result carries %q: %s", key, data["votes"])
		}
	}

	castSealedSeat(t, conn, pid, "seat-b")
	data, votes = result()
	if string(data["status"]) != `"approved"` || len(votes) != 2 {
		t.Fatalf("after quorum: status=%s votes=%d, want approved and 2", data["status"], len(votes))
	}
	for _, v := range votes {
		if v["verdict"] != string(model.VerdictApproveWithConcerns) || v["summary"] == nil {
			t.Errorf("closed result withholds %v's cast: %v", v["voter_name"], v)
		}
	}
}
