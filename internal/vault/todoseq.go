package vault

import (
	"regexp"
	"strings"
	"time"
)

var todoLineRe = regexp.MustCompile(`^([ \t]*)(?:(?:[-+*]|\d+[.)])[ \t]+)?(?:\[([ xX>/-])\][ \t]+)?(TODO|LATER|NOW|DOING|IN-PROGRESS|WAIT|WAITING|DONE|CANCELED|CANCELLED|ARCHIVED)([ \t]+)(.*)$`)
var todoPriorityRe = regexp.MustCompile(`(?:^|\s)\[#([ABC])\](?:\s|$)`)
var todoPlanningStart = regexp.MustCompile(`^\s*(SCHEDULED|DEADLINE|CLOSED):`)
var todoEntryRe = regexp.MustCompile(`(SCHEDULED|DEADLINE|CLOSED):\s*(<[^>]*>|\[[^\]]*\])`)
var todoTerminator = regexp.MustCompile(`^\s*#{1,6}(?:\s|$)`)
var todoFence = regexp.MustCompile("^(\\s*)(```|~~~)")
var todoStatuses = map[string]string{"TODO": "todo", "LATER": "todo", "NOW": "doing", "DOING": "doing", "IN-PROGRESS": "doing", "WAIT": "waiting", "WAITING": "waiting", "DONE": "done", "CANCELED": "cancelled", "CANCELLED": "cancelled"}

type todoScan struct {
	line, index, checkboxIndex, planningEnd int
	match                                   []string
	children                                []int
}

func todoIndent(line string) int {
	n := 0
	for _, c := range line {
		if c == ' ' {
			n++
		} else if c == '\t' {
			n += 2
		} else {
			break
		}
	}
	return n
}
func scanTodoSeq(body string) []todoScan {
	lines := strings.Split(body, "\n")
	scans := []todoScan{}
	fence := ""
	checkboxIndex, index := 0, 0
	fmEnd := -1
	if strings.HasPrefix(body, "---\n") {
		for i := 1; i < len(lines); i++ {
			if lines[i] == "---" {
				fmEnd = i
				break
			}
		}
	}
	for i, line := range lines {
		if f := todoFence.FindStringSubmatch(line); f != nil {
			if fence == f[2] {
				fence = ""
			} else if fence == "" {
				fence = f[2]
			}
			continue
		}
		if fence != "" {
			continue
		}
		ci := -1
		if taskLineRe.MatchString(line) {
			ci = checkboxIndex
			checkboxIndex++
		}
		m := todoLineRe.FindStringSubmatch(line)
		if m == nil || i <= fmEnd {
			continue
		}
		end := i + 1
		for end < len(lines) && todoIndent(lines[end]) >= todoIndent(line) && todoPlanningStart.MatchString(lines[end]) {
			end++
		}
		scan := todoScan{line: i, index: -1, checkboxIndex: ci, planningEnd: end, match: m}
		if ci < 0 {
			scan.index = index
			index++
		}
		childFence := ""
		for j := end; j < len(lines); j++ {
			child := lines[j]
			if strings.TrimSpace(child) == "" {
				continue
			}
			f := todoFence.FindStringSubmatch(child)
			if childFence != "" {
				if f != nil && f[2] == childFence {
					childFence = ""
				}
				continue
			}
			if f != nil {
				if todoIndent(child) <= todoIndent(line) {
					break
				}
				childFence = f[2]
				continue
			}
			if todoLineRe.MatchString(child) || todoTerminator.MatchString(child) {
				break
			}
			depth, parentDepth := todoIndent(child), todoIndent(line)
			if taskLineRe.MatchString(child) && !strings.HasPrefix(strings.TrimSpace(child), ">") {
				if depth < parentDepth || (m[2] != "" && depth == parentDepth) {
					break
				}
				scan.children = append(scan.children, j)
			} else if depth <= parentDepth {
				break
			}
		}
		scans = append(scans, scan)
	}
	return scans
}
func applyTodoSeq(task *Task, scan todoScan, lines []string, path string) {
	keyword := scan.match[3]
	status := todoStatuses[keyword]
	task.Syntax = "todoseq"
	task.Keyword = keyword
	if scan.checkboxIndex < 0 {
		task.ID = path + "#todoseq:" + itoa(scan.index)
		task.TaskIndex = -2 - scan.index
	}
	task.Status = status
	task.Fields["status"] = status
	task.Checked = status == "done"
	task.Cancelled = status == "cancelled"
	task.InProgress = status == "doing"
	task.Waiting = status == "waiting"
	task.Forwarded = false
	task.Due = ""
	task.Priority = ""
	if p := todoPriorityRe.FindStringSubmatch(scan.match[5]); p != nil {
		task.Priority = map[string]string{"A": "high", "B": "med", "C": "low"}[p[1]]
	}
	for _, line := range lines[scan.line+1 : scan.planningEnd] {
		for _, e := range todoEntryRe.FindAllStringSubmatch(line, -1) {
			if len(e[2]) < 12 || !strings.ContainsRune(" \t>]", rune(e[2][11])) || ((e[1] == "CLOSED") != strings.HasPrefix(e[2], "[")) {
				continue
			}
			date := e[2][1:11]
			if _, err := time.Parse("2006-01-02", date); err != nil {
				continue
			}
			switch e[1] {
			case "DEADLINE":
				task.Due = date
			case "SCHEDULED":
				task.Scheduled = date
			case "CLOSED":
				task.CompletedDate = date
			}
		}
	}
}
func attachTodoChildren(tasks []Task, scans []todoScan) []Task {
	hidden := map[int]bool{}
	for _, scan := range scans {
		pi := -1
		for i := range tasks {
			if tasks[i].LineNumber == scan.line && tasks[i].Syntax == "todoseq" {
				pi = i
				break
			}
		}
		if pi < 0 {
			continue
		}
		if scan.match[3] == "ARCHIVED" {
			hidden[scan.line] = true
			for _, line := range scan.children {
				hidden[line] = true
			}
			continue
		}
		progress := &TaskProgress{}
		for _, line := range scan.children {
			for i := range tasks {
				if tasks[i].LineNumber == line && tasks[i].Kind != "file" {
					tasks[i].ParentID = tasks[pi].ID
					if !tasks[i].Cancelled && !tasks[i].Forwarded {
						progress.Total++
						if tasks[i].Checked {
							progress.Done++
						}
					}
				}
			}
		}
		tasks[pi].Progress = progress
	}
	out := []Task{}
	for _, task := range tasks {
		if task.Kind == "file" || !hidden[task.LineNumber] {
			out = append(out, task)
		}
	}
	return out
}
