package repository

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No Go comment says a paragraph twice, one after the other: a paragraph
// pasted again and then reworded, as one about a job's source labels once
// was, reads as two things where there is one, and the next edit changes one
// of them and leaves the other saying what is no longer so. Two paragraphs
// of one comment, one after the other, are the same when four in five of
// the shorter one's words, each counted as often as it is written, are
// the longer one's too.
func TestNoGoCommentSaysAParagraphTwice(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, comment := range commentParagraphs(string(raw)) {
			for i := 1; i < len(comment); i++ {
				if sameParagraph(comment[i-1], comment[i]) {
					t.Errorf("%s says a paragraph twice:\n%s\n\n%s", path, comment[i-1], comment[i])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// commentParagraphs are src's comments of // lines, each as its paragraphs,
// which an empty // line ends.
func commentParagraphs(src string) [][]string {
	var comments [][]string
	var comment []string
	var paragraph []string
	endParagraph := func() {
		if len(paragraph) > 0 {
			comment = append(comment, strings.Join(paragraph, " "))
		}
		paragraph = nil
	}
	for line := range strings.SplitSeq(src, "\n") {
		text, ok := strings.CutPrefix(strings.TrimSpace(line), "//")
		if !ok {
			endParagraph()
			if len(comment) > 1 {
				comments = append(comments, comment)
			}
			comment = nil
			continue
		}
		if text = strings.TrimSpace(text); text == "" {
			endParagraph()
			continue
		}
		paragraph = append(paragraph, text)
	}
	endParagraph()
	if len(comment) > 1 {
		comments = append(comments, comment)
	}
	return comments
}

// sameParagraph is whether four in five of the words of the shorter of a
// and b are the other's, a paragraph of fewer than 12 words never being
// the same as another: a short line, as a list's, may well repeat most of
// the one before.
func sameParagraph(a, b string) bool {
	short, long := strings.Fields(a), strings.Fields(b)
	if len(short) > len(long) {
		short, long = long, short
	}
	if len(short) < 12 {
		return false
	}
	words := map[string]int{}
	for _, w := range long {
		words[strings.ToLower(w)]++
	}
	shared := 0
	for _, w := range short {
		if w = strings.ToLower(w); words[w] > 0 {
			words[w]--
			shared++
		}
	}
	return shared*5 >= len(short)*4
}
