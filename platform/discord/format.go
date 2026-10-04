package discord

import (
	"fmt"
	"regexp"
	"strings"
)

// Discord does not render GFM pipe tables. Hermes rewrites them at send
// time into bold headings + bullets (convert_table_to_bullets). jung-connect
// matches that on the outbound path; the model is not told to avoid tables.

var tableSeparatorRe = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*){1,}\|?\s*$`)

func convertTablesToBullets(text string) string {
	if !strings.Contains(text, "|") || !strings.Contains(text, "-") {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	i := 0
	for i < len(lines) {
		line := lines[i]
		isFence := strings.HasPrefix(strings.TrimLeft(line, " \t"), "```")
		if isFence {
			inFence = !inFence
		}
		if !inFence && !isFence && strings.Contains(line, "|") && i+1 < len(lines) && tableSeparatorRe.MatchString(lines[i+1]) {
			j := i + 2
			for j < len(lines) && isTableRow(lines[j]) {
				j++
			}
			out = append(out, renderTableBlock(lines[i:j]))
			i = j
			continue
		}
		out = append(out, line)
		i++
	}
	return strings.Join(out, "\n")
}

func isTableRow(line string) bool {
	return strings.Contains(strings.TrimSpace(line), "|")
}

func splitTableRow(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	parts := strings.Split(s, "|")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = strings.TrimSpace(p)
	}
	return out
}

func renderTableBlock(tableBlock []string) string {
	if len(tableBlock) < 3 {
		return strings.Join(tableBlock, "\n")
	}
	headers := splitTableRow(tableBlock[0])
	if len(headers) < 2 {
		return strings.Join(tableBlock, "\n")
	}
	hasRowLabelCol := len(splitTableRow(tableBlock[2])) == len(headers)+1
	var groups []string
	for index, row := range tableBlock[2:] {
		cells := splitTableRow(row)
		var heading string
		var data []string
		if hasRowLabelCol {
			if len(cells) > 0 && cells[0] != "" {
				heading = cells[0]
			} else {
				heading = fmt.Sprintf("Row %d", index+1)
			}
			if len(cells) > 1 {
				data = cells[1:]
			}
		} else {
			heading = fmt.Sprintf("Row %d", index+1)
			for _, cell := range cells {
				if cell != "" {
					heading = cell
					break
				}
			}
			data = cells
		}
		padded := make([]string, len(headers))
		copy(padded, data)
		var bullets []string
		for i, header := range headers {
			value := padded[i]
			if hasRowLabelCol || value != heading {
				bullets = append(bullets, "• "+header+": "+value)
			}
		}
		group := "**" + heading + "**"
		if len(bullets) > 0 {
			group += "\n" + strings.Join(bullets, "\n")
		}
		groups = append(groups, group)
	}
	return strings.Join(groups, "\n\n")
}
