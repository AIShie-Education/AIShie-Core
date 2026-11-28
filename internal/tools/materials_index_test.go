package tools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBodyMaterialPassages(t *testing.T) {
	body := "# 排序\n\n" + strings.Repeat("合併排序的複雜度。\n", 90) + "\n## 後續\n" + strings.Repeat("快速排序。\n", 90)
	passages := bodyMaterialPassages(body)
	if len(passages) < 2 {
		t.Fatalf("a long body was not cut into passages: %d", len(passages))
	}
	for i, p := range passages {
		if p.kind != "body" || p.section != 0 || p.part != i+1 {
			t.Errorf("passage %d: %+v", i, p)
		}
		if n := utf8.RuneCountInString(p.content); n == 0 || n > materialChunkRunes {
			t.Errorf("passage %d has %d runes", i, n)
		}
		if !strings.Contains(body, p.content) {
			t.Errorf("passage %d is not original content", i)
		}
	}
	if !strings.HasPrefix(passages[0].content, "# 排序") {
		t.Errorf("the first passage lost its heading: %q", passages[0].content)
	}
	foundNext := false
	for _, p := range passages {
		foundNext = foundNext || strings.HasPrefix(p.content, "## 後續")
	}
	if !foundNext {
		t.Error("no passage starts at the later Markdown heading")
	}
	if got := bodyMaterialPassages(" \n\t\n"); len(got) != 0 {
		t.Errorf("whitespace alone made passages: %+v", got)
	}
}

func TestFileMaterialPassagesAlignWithTextParts(t *testing.T) {
	body := "前言：排序\n\n## Slide 1: 合併排序\n" + strings.Repeat("合併排序的複雜度是線性對數。\n", 5000) +
		"\n## Page 2: Comparison\nMerge sort splits the list.\n\n## 第 3 頁：總結\n最後複習。"
	parts := textParts(body)
	if len(parts) < 2 {
		t.Fatal("the fixture does not exercise more than one document.text part")
	}
	passages := fileMaterialPassages(body)
	if len(passages) < len(parts) {
		t.Fatalf("%d passages for %d parts", len(passages), len(parts))
	}
	var slide, englishPage, hanPage bool
	for _, p := range passages {
		if p.part < 1 || p.part > len(parts) {
			t.Fatalf("part %d is outside document.text's %d parts", p.part, len(parts))
		}
		if !strings.Contains(parts[p.part-1], p.content) {
			t.Errorf("passage crosses part %d: %q", p.part, p.content)
		}
		if n := utf8.RuneCountInString(p.content); n == 0 || n > materialChunkRunes {
			t.Errorf("part %d has a passage of %d runes", p.part, n)
		}
		switch {
		case p.kind == "slide" && p.section == 1:
			slide = true
		case p.kind == "page" && p.section == 2 && strings.Contains(p.content, "Merge sort"):
			englishPage = true
		case p.kind == "page" && p.section == 3 && strings.Contains(p.content, "最後複習"):
			hanPage = true
		}
	}
	if !slide || !englishPage || !hanPage {
		t.Errorf("sections not retained: slide=%v page=%v 第3頁=%v", slide, englishPage, hanPage)
	}
	if passages[0].kind != "body" || passages[0].part != 1 {
		t.Errorf("text before the first section: %+v", passages[0])
	}
	// A page or slide continued by textParts keeps its section in the new part.
	for i, p := range passages[1:] {
		if p.part > passages[i].part && p.kind != "body" && !strings.HasPrefix(p.content, "##") {
			if p.kind != passages[i].kind || p.section != passages[i].section {
				t.Errorf("continuation changed section: %+v then %+v", passages[i], p)
			}
		}
	}
}

func TestMaterialSectionHeadings(t *testing.T) {
	for _, heading := range []string{"## Slide 2", "## slide 10: Sorting", "## Page 3 — detail", "## 第 4 頁", "## 第5頁：複習"} {
		if _, _, ok := materialSection(heading); !ok {
			t.Errorf("%q was not recognised", heading)
		}
	}
	for _, heading := range []string{"# Slide 1", "### Slide 1", "## Slides 1", "## 第 頁", "Slide 1"} {
		if _, _, ok := materialSection(heading); ok {
			t.Errorf("%q is not a page or slide heading", heading)
		}
	}
}
