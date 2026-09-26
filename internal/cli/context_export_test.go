package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestCapsuleFileCarriesEveryRecord protects the one invariant `codectx context
// export` exists for: the file it writes is the capsule WHOLE.
//
// A sealed capsule keeps counts in its blob and its records as durable rows, so
// the export writer streams each of the eight lists page by page. The two
// failure modes that leaves are both silent: a writer that encodes the capsule
// record alone produces a well-formed file with no records in it, and a writer
// that stops at the first page produces one that is short. Neither shows up as
// an error at the boundary -- only a record-for-record comparison against the
// sealed counts catches them -- and the artifact a later session replays would
// be incomplete either way.
//
// The pager below hands out two-record pages, so the scope list alone spans
// three of them and the walk is exercised rather than assumed.
func TestCapsuleFileCarriesEveryRecord(t *testing.T) {
	rows := map[model.CapsuleList][]model.CapsuleRow{}
	var counts model.CapsuleCounts
	for i, list := range model.CapsuleListOrder {
		n := int64(i + 1)
		for ordinal := range n {
			// The rows are built directly rather than through
			// model.NewCapsuleRow: the writer's whole contract is over the
			// key and the stored bytes, which is exactly what the store hands
			// back, and a typed record per list would assert the model's
			// encoding a second time.
			key := fmt.Sprintf("%s-%d", list, ordinal)
			rows[list] = append(rows[list], model.CapsuleRow{
				List: list, Ordinal: ordinal, Key: key,
				JSON: []byte(fmt.Sprintf(`{"list":%q,"key":%q}`, list, key)),
			})
		}
		counts.Set(list, n)
	}
	capsule := model.Capsule{
		SessionID:     model.SessionID(strings.Repeat("a", 64)),
		ActorID:       "actor-1",
		ManifestHash:  strings.Repeat("d", 64),
		CanonicalHash: strings.Repeat("e", 64),
		ScopeVersion:  3,
		Counts:        counts,
	}
	const pageSize = 2
	pager := func(list model.CapsuleList, after string) ([]model.CapsuleRow, string, error) {
		all := rows[list]
		start := 0
		if after != "" {
			for i, row := range all {
				if row.Key == after {
					start = i + 1
					break
				}
			}
		}
		end := min(start+pageSize, len(all))
		page := all[start:end]
		next := ""
		if end < len(all) && len(page) > 0 {
			next = page[len(page)-1].Key
		}
		return page, next, nil
	}

	path := filepath.Join(t.TempDir(), "capsule.json")
	written, err := writeCapsuleFile(path, capsule, pager)
	if err != nil {
		t.Fatalf("write the capsule file: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the capsule file back: %v", err)
	}
	if int64(len(raw)) != written {
		t.Errorf("the command reported %d bytes written; the file holds %d", written, len(raw))
	}
	var got struct {
		Capsule model.Capsule                             `json:"capsule"`
		Records map[model.CapsuleList][]map[string]string `json:"records"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the exported capsule is not readable JSON: %v\n%s", err, raw)
	}
	if got.Capsule.CanonicalHash != capsule.CanonicalHash {
		t.Errorf("the file names capsule %s, not the exported %s", got.Capsule.CanonicalHash, capsule.CanonicalHash)
	}
	for _, list := range model.CapsuleListOrder {
		if int64(len(got.Records[list])) != counts.Of(list) {
			t.Errorf("the file carries %d %s records; the capsule sealed %d",
				len(got.Records[list]), list, counts.Of(list))
		}
		for i, record := range got.Records[list] {
			want := fmt.Sprintf("%s-%d", list, i)
			if record["list"] != string(list) || record["key"] != want {
				t.Errorf("%s record %d is %v, want the stored row %s", list, i, record, want)
			}
		}
	}
}

// TestCapsuleFileRefusesAShortList protects the refusal that makes the file
// above trustworthy: a list that streams fewer records than the seal counted
// stops the export and takes the partial file with it, rather than leaving an
// artifact that reads as a complete capsule of a smaller session.
func TestCapsuleFileRefusesAShortList(t *testing.T) {
	capsule := model.Capsule{
		SessionID:     model.SessionID(strings.Repeat("a", 64)),
		ActorID:       "actor-1",
		ManifestHash:  strings.Repeat("d", 64),
		CanonicalHash: strings.Repeat("e", 64),
		ScopeVersion:  1,
		Counts:        model.CapsuleCounts{Scope: 4},
	}
	path := filepath.Join(t.TempDir(), "capsule.json")
	_, err := writeCapsuleFile(path, capsule, func(model.CapsuleList, string) ([]model.CapsuleRow, string, error) {
		return nil, "", nil
	})
	if err == nil {
		t.Fatal("a capsule whose scope list streamed no record was exported as complete")
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeInternal {
		t.Fatalf("export failed with %v, want a typed %s refusal", err, model.CodeInternal)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("the short export left %s on disk; a partial capsule must never survive", path)
	}
}
