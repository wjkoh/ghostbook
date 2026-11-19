package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wjkoh/ghostbook/internal/renderer"
)

func (e *BookEngine) parseOrderedChapterEntriesLocked() ([]renderer.SummaryEntry, string, error) {
	rawSummary, err := e.readSrcFileSafe("SUMMARY.md")
	if err != nil {
		return nil, "", fmt.Errorf("read SUMMARY.md: %w", err)
	}
	summaryText := string(rawSummary)
	parsed := renderer.ParseSummaryMarkdown(summaryText)

	seen := make(map[string]bool, len(parsed))
	var entries []renderer.SummaryEntry
	for _, item := range parsed {
		slug, err := ValidateSlug(item.Slug)
		if err == nil && !seen[slug] {
			seen[slug] = true
			item.Slug = slug
			entries = append(entries, item)
		}
	}

	dirEntries, err := os.ReadDir(filepath.Join(e.repoDir, "src"))
	if err == nil {
		var extras []string
		for _, ent := range dirEntries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".md") {
				continue
			}
			base := strings.TrimSuffix(ent.Name(), ".md")
			if base == "SUMMARY" || base == "STYLE_GUIDE" {
				continue
			}
			if slug, err := ValidateSlug(base); err == nil && !seen[slug] {
				seen[slug] = true
				extras = append(extras, slug)
			}
		}
		sort.Strings(extras)
		for _, slug := range extras {
			entries = append(entries, renderer.SummaryEntry{Slug: slug})
		}
	}

	return entries, summaryText, nil
}

func formatInitialChapterBody(title, initialMarkdown string) string {
	body := strings.TrimSpace(initialMarkdown)
	if body == "" {
		return "# " + title + "\n\nDrafting in progress.\n"
	}
	if !strings.HasPrefix(body, "# ") {
		return "# " + title + "\n\n" + body + "\n"
	}
	return body + "\n"
}

func insertChildSummaryEntry(summaryStr, entryLine, cleanParent string) (string, bool) {
	parentTarget := "(" + cleanParent + ".md)"
	parentRelTarget := "(./" + cleanParent + ".md)"
	lines := strings.Split(summaryStr, "\n")
	for idx, line := range lines {
		if !strings.Contains(line, parentTarget) && !strings.Contains(line, parentRelTarget) {
			continue
		}
		indent := ""
		trimmedLeft := strings.TrimLeft(line, " \t")
		if len(line) > len(trimmedLeft) {
			indent = line[:len(line)-len(trimmedLeft)]
		}
		childIndent := indent + "  "
		insertAt := idx + 1
		for insertAt < len(lines) {
			nextLine := lines[insertAt]
			if strings.TrimSpace(nextLine) == "" {
				break
			}
			nextTrimmed := strings.TrimLeft(nextLine, " \t")
			if len(nextLine)-len(nextTrimmed) <= len(indent) {
				break
			}
			insertAt++
		}
		updated := make([]string, 0, len(lines)+1)
		updated = append(updated, lines[:insertAt]...)
		updated = append(updated, childIndent+entryLine)
		updated = append(updated, lines[insertAt:]...)
		return strings.Join(updated, "\n"), true
	}
	return summaryStr, false
}

func insertPartSummaryEntry(summaryStr, entryLine, cleanPart string) string {
	lines := strings.Split(summaryStr, "\n")
	partIdx := -1
	substringIdx := -1
	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			headingText := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			if strings.EqualFold(headingText, "Summary") {
				continue
			}
			if strings.EqualFold(headingText, cleanPart) {
				partIdx = idx
				break
			}
			if substringIdx < 0 && strings.Contains(strings.ToLower(headingText), strings.ToLower(cleanPart)) {
				substringIdx = idx
			}
		}
	}
	if partIdx < 0 {
		partIdx = substringIdx
	}
	if partIdx >= 0 {
		insertAt := partIdx + 1
		lastBulletIdx := partIdx
		for insertAt < len(lines) {
			trimmed := strings.TrimSpace(lines[insertAt])
			if strings.HasPrefix(trimmed, "#") {
				break
			}
			if trimmed != "" {
				lastBulletIdx = insertAt
			}
			insertAt++
		}
		updated := make([]string, 0, len(lines)+1)
		updated = append(updated, lines[:lastBulletIdx+1]...)
		updated = append(updated, entryLine)
		updated = append(updated, lines[lastBulletIdx+1:]...)
		return strings.Join(updated, "\n")
	}
	if !strings.HasSuffix(summaryStr, "\n") {
		summaryStr += "\n"
	}
	return summaryStr + "\n# " + cleanPart + "\n" + entryLine + "\n"
}

func sanitizeSummaryTitle(title string) string {
	clean := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "[", "(", "]", ")").Replace(title))
	if clean == "" {
		return "Untitled"
	}
	return clean
}

func insertSummaryEntry(summaryStr, title, slug, parentSlug, partTitle string) string {
	if strings.Contains(summaryStr, "("+slug+".md)") || strings.Contains(summaryStr, "(./"+slug+".md)") {
		return summaryStr
	}
	entryLine := fmt.Sprintf("- [%s](%s.md)", sanitizeSummaryTitle(title), slug)

	if cleanParent, err := ValidateSlug(parentSlug); err == nil && cleanParent != "" {
		if out, ok := insertChildSummaryEntry(summaryStr, entryLine, cleanParent); ok {
			return out
		}
	}

	cleanPart := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(partTitle), "#"))
	if cleanPart != "" {
		return insertPartSummaryEntry(summaryStr, entryLine, cleanPart)
	}

	if !strings.HasSuffix(summaryStr, "\n") {
		summaryStr += "\n"
	}
	return summaryStr + entryLine + "\n"
}

func replaceFirstH1Heading(md, newTitle string) string {
	lines := strings.Split(md, "\n")
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isMarkdownCodeFence(trimmed) {
			inFence = !inFence
			continue
		}
		if !inFence && strings.HasPrefix(trimmed, "# ") {
			lines[i] = "# " + newTitle
			return strings.Join(lines, "\n")
		}
	}
	return "# " + newTitle + "\n\n" + md
}

func rewriteSummaryForRename(summaryStr, oldSlug, newSlug, newTitle string) string {
	lines := strings.Split(summaryStr, "\n")
	oldTarget := "(" + oldSlug + ".md)"
	oldRelTarget := "(./" + oldSlug + ".md)"
	found := false

	for i, line := range lines {
		if strings.Contains(line, oldTarget) || strings.Contains(line, oldRelTarget) {
			found = true
			updated := strings.ReplaceAll(line, oldTarget, "("+newSlug+".md)")
			updated = strings.ReplaceAll(updated, oldRelTarget, "(./"+newSlug+".md)")
			if newTitle != "" {
				safeTitle := sanitizeSummaryTitle(newTitle)
				openIdx := strings.IndexByte(updated, '[')
				closeIdx := strings.LastIndex(updated, "](")
				if openIdx >= 0 && closeIdx > openIdx {
					updated = updated[:openIdx+1] + safeTitle + updated[closeIdx:]
				}
			}
			lines[i] = updated
		}
	}
	out := strings.Join(lines, "\n")
	if !found && newSlug != "" {
		titleToUse := newTitle
		if titleToUse == "" {
			titleToUse = newSlug
		}
		out = insertSummaryEntry(out, titleToUse, newSlug, "", "")
	}
	return out
}

func removeSummaryEntry(summaryStr, slug string) string {
	lines := strings.Split(summaryStr, "\n")
	target1 := "(" + slug + ".md)"
	target2 := "(./" + slug + ".md)"
	kept := make([]string, 0, len(lines))
	removedIndent := -1

	for _, line := range lines {
		trimmedLeft := strings.TrimLeft(line, " \t")
		curIndent := len(line) - len(trimmedLeft)

		if strings.Contains(line, target1) || strings.Contains(line, target2) {
			removedIndent = curIndent
			continue
		}
		if removedIndent >= 0 {
			if strings.TrimSpace(line) == "" || curIndent <= removedIndent {
				removedIndent = -1
			} else if strings.HasPrefix(line, "  ") {
				line = line[2:]
			} else if strings.HasPrefix(line, "\t") {
				line = line[1:]
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func replaceChapterSlugLinks(text, oldSlug, newSlug string) string {
	oldPrefix := "/chapters/" + oldSlug
	newPrefix := "/chapters/" + newSlug
	var b strings.Builder
	b.Grow(len(text))

	rem := text
	for {
		idx := strings.Index(rem, oldPrefix)
		if idx < 0 {
			b.WriteString(rem)
			break
		}
		b.WriteString(rem[:idx])
		after := rem[idx+len(oldPrefix):]
		shouldRewrite := false
		if after == "" {
			shouldRewrite = true
		} else if strings.HasPrefix(after, ".md") || strings.HasPrefix(after, "/diagrams/") || strings.HasPrefix(after, "/history") {
			shouldRewrite = true
		} else {
			c := after[0]
			// Rewrite live links (#anchor, ?query, ), ], >, ", ', whitespace, punctuation) while preserving prefix-sharing slugs (-, alnum) and historical /rev/ permalinks
			if c == '#' || c == '?' || c == ')' || c == ']' || c == '>' || c == '"' || c == '\'' || c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '.' || c == ',' || c == ';' || c == ':' {
				shouldRewrite = true
			}
		}
		if shouldRewrite {
			b.WriteString(newPrefix)
		} else {
			b.WriteString(oldPrefix)
		}
		rem = after
	}
	return b.String()
}

func (e *BookEngine) rewriteCrossChapterLinksLocked(oldSlug, newSlug string) []string {
	if oldSlug == newSlug {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(e.repoDir, "src"))
	if err != nil {
		return nil
	}
	var modifiedRelPaths []string
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".md") || ent.Name() == "SUMMARY.md" {
			continue
		}
		raw, readErr := e.readSrcFileSafe(ent.Name())
		if readErr != nil {
			continue
		}
		orig := string(raw)
		updated := strings.ReplaceAll(orig, "("+oldSlug+".md)", "("+newSlug+".md)")
		updated = strings.ReplaceAll(updated, "(./"+oldSlug+".md)", "(./"+newSlug+".md)")
		updated = strings.ReplaceAll(updated, "("+oldSlug+".md#", "("+newSlug+".md#")
		updated = strings.ReplaceAll(updated, "(./"+oldSlug+".md#", "(./"+newSlug+".md#")
		updated = replaceChapterSlugLinks(updated, oldSlug, newSlug)
		if updated != orig {
			if writeErr := e.writeSrcFileSafe(ent.Name(), []byte(updated)); writeErr == nil {
				modifiedRelPaths = append(modifiedRelPaths, filepath.ToSlash(filepath.Join("src", ent.Name())))
			}
		}
	}
	return modifiedRelPaths
}
