package fetch

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// An entry of request.accept_status that is a status written with a sign or
// leading zeros, which the load takes as the status it reads, is written as
// the status by its digits alone, of a collector and of a static target, so
// that it accepts that status: "0503" and "+503", in quotes, passed the load
// and matched no status, since AcceptedStatus compares the text.
func TestAStatusWithASignOrLeadingZerosAcceptsTheStatusItIs(t *testing.T) {
	entries := []string{"0503", " +503 ", "+0200", "0404", "2XX"}
	if err := normalizeAcceptStatus(entries); err != nil {
		t.Fatal(err)
	}
	if want := []string{"503", "503", "200", "404", "2xx"}; !slices.Equal(entries, want) {
		t.Errorf("the entries are %q, want %q", entries, want)
	}
	c := &model.Collector{Request: model.RequestConfig{AcceptStatus: entries}}
	for _, status := range []int{503, 200, 404, 201} {
		if !AcceptedStatus(c, RequestOverrides{}, status) {
			t.Errorf("%d is not accepted by %q", status, entries)
		}
	}
	target := model.StaticTarget{Request: model.TargetRequestConfig{AcceptStatus: []string{"0503", "+503", " 0404 ", "5XX"}}}
	NormalizeTargetRequest(&target)
	if want := []string{"503", "503", "404", "5xx"}; !slices.Equal(target.Request.AcceptStatus, want) {
		t.Errorf("the target's entries are %q, want %q", target.Request.AcceptStatus, want)
	}
	if err := checkAcceptStatus(target.Request.AcceptStatus); err != nil {
		t.Error(err)
	}
	if !AcceptedStatus(&model.Collector{}, TargetOverrides(&target), 503) {
		t.Errorf("503 is not accepted by the target's %q", target.Request.AcceptStatus)
	}
}

// normalizeAcceptStatusAsItWas is normalizeAcceptStatus before a status
// written with a sign or leading zeros was written by its digits alone, kept
// as the oracle.
func normalizeAcceptStatusAsItWas(entries []string) error {
	for i, raw := range entries {
		entry := strings.ToLower(strings.TrimSpace(raw))
		entries[i] = entry
		if len(entry) == 3 && entry[1:] == "xx" && entry[0] >= '1' && entry[0] <= '5' {
			continue
		}
		if code, err := strconv.Atoi(entry); err == nil && code >= 100 && code <= 599 {
			continue
		}
		return fmt.Errorf("entry %q is not an HTTP status from 100 to 599 or a class such as 2xx", raw)
	}
	return nil
}

// Over generated entries of digits, signs, blanks, points, letters and
// classes, the load of request.accept_status, of a collector and of a static
// target, takes and refuses what it did in the words it did, and writes each
// entry as it did, but for a status written with a sign or leading zeros,
// which is now its digits alone.
func TestAnAcceptStatusEntryIsReadAsItWasButForASignOrLeadingZeros(t *testing.T) {
	random := rand.New(rand.NewPCG(117, 2))
	alphabet := []string{"0", "1", "2", "5", "9", "+", "-", " ", ".", "x", "X", "e", "_"}
	table := []string{"200", "0200", "+200", "-200", "00200", "2xx", "2XX", " 503 ", "600", "099", "0700", "", "+", "1e2", "200.0", "0x1F7", "5_03", "+0", "6xx"}
	var changed int
	for round := range alloctest.UnlessRaced(20000, 4000) {
		var entries []string
		for range 1 + random.IntN(3) {
			if random.IntN(3) == 0 {
				entries = append(entries, table[random.IntN(len(table))])
				continue
			}
			var entry strings.Builder
			for range random.IntN(6) {
				entry.WriteString(alphabet[random.IntN(len(alphabet))])
			}
			entries = append(entries, entry.String())
		}
		now, was := slices.Clone(entries), slices.Clone(entries)
		err, wasErr := normalizeAcceptStatus(now), normalizeAcceptStatusAsItWas(was)
		if fmt.Sprint(err) != fmt.Sprint(wasErr) {
			t.Fatalf("round %d, %q: %v, and was %v", round, entries, err, wasErr)
		}
		target := model.StaticTarget{Request: model.TargetRequestConfig{AcceptStatus: slices.Clone(entries)}}
		NormalizeTargetRequest(&target)
		// The load stops at the entry it refuses, and writes none after it.
		refused := len(entries)
		for i, entry := range entries {
			if normalizeAcceptStatusAsItWas([]string{entry}) != nil {
				refused = i
				break
			}
		}
		for i := range now {
			want := was[i]
			if code, atoiErr := strconv.Atoi(want); i <= refused && atoiErr == nil && code >= 100 && code <= 599 && want != strconv.Itoa(code) {
				want = strconv.Itoa(code)
				changed++
			}
			if now[i] != want {
				t.Fatalf("round %d, %q: entry %d is %q, and was %q", round, entries, i, now[i], was[i])
			}
		}
		// The target's are written whatever the load then says of them.
		for i, entry := range entries {
			want := strings.ToLower(strings.TrimSpace(entry))
			if code, atoiErr := strconv.Atoi(want); atoiErr == nil && code >= 100 && code <= 599 {
				want = strconv.Itoa(code)
			}
			if target.Request.AcceptStatus[i] != want {
				t.Fatalf("round %d, %q: the target's entry %d is %q, want %q", round, entries, i, target.Request.AcceptStatus[i], want)
			}
		}
	}
	if changed == 0 {
		t.Fatal("no entry was a status with a sign or leading zeros")
	}
}
