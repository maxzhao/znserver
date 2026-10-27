package vault

import (
	"errors"
	"sort"
	"strings"
	"time"
)

type PendingMessage struct {
	ID        string `json:"id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"createdAt"`
}
type PendingThread struct {
	Path        string           `json:"path"`
	ThreadID    string           `json:"threadId"`
	AnchorText  string           `json:"anchorText"`
	AnchorLine  int              `json:"anchorLine"`
	AnchorFound bool             `json:"anchorFound"`
	Messages    []PendingMessage `json:"messages"`
}

func (v *Vault) PendingComments(author string) ([]PendingThread, error) {
	notes, err := v.ListNotes()
	if err != nil {
		return nil, err
	}
	out := []PendingThread{}
	for _, n := range notes {
		if n.Folder == FolderTrash {
			continue
		}
		comments, err := v.ReadNoteComments(n.Path)
		if err != nil {
			return nil, err
		}
		content, err := v.ReadNote(n.Path)
		if err != nil {
			return nil, err
		}
		for _, root := range comments {
			if root.ParentID != "" || root.ResolvedAt != nil {
				continue
			}
			messages := []NoteComment{root}
			for _, c := range comments {
				if c.ParentID == root.ID {
					messages = append(messages, c)
				}
			}
			sort.SliceStable(messages, func(i, j int) bool { return messages[i].CreatedAt < messages[j].CreatedAt })
			if messages[len(messages)-1].Author == author {
				continue
			}
			item := PendingThread{Path: n.Path, ThreadID: root.ID, AnchorText: root.AnchorText, Messages: []PendingMessage{}}
			pos := strings.Index(content.Body, root.AnchorText)
			item.AnchorFound = root.AnchorText != "" && pos >= 0
			if item.AnchorFound {
				item.AnchorLine = strings.Count(content.Body[:pos], "\n") + 1
			} else {
				item.AnchorLine = strings.Count(content.Body[:min(max(root.AnchorStart, 0), len(content.Body))], "\n") + 1
			}
			for _, m := range messages {
				item.Messages = append(item.Messages, PendingMessage{m.ID, m.Author, m.Body, m.CreatedAt})
			}
			out = append(out, item)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Messages, out[j].Messages
		return a[len(a)-1].CreatedAt < b[len(b)-1].CreatedAt
	})
	return out, nil
}

type CommentMutation struct {
	Path      string      `json:"path"`
	Operation string      `json:"operation"`
	Comment   NoteComment `json:"comment"`
	ID        string      `json:"id"`
	Resolved  bool        `json:"resolved"`
}

var ErrInvalidCommentMutation = errors.New("invalid comment mutation")

// Sidecar read-modify-write is one server operation under the vault lock.
func (v *Vault) MutateComment(req CommentMutation) ([]NoteComment, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, err := v.readNoteLocked(req.Path); err != nil {
		return nil, err
	}
	comments, err := v.readNoteCommentsLocked(req.Path)
	if err != nil {
		return nil, err
	}
	switch req.Operation {
	case "add", "reply":
		c := req.Comment
		if c.ID == "" {
			c.ID = newCommentID()
		}
		for _, old := range comments {
			if old.ID == c.ID {
				return nil, ErrInvalidCommentMutation
			}
		}
		now := time.Now().UnixMilli()
		for _, old := range comments {
			if old.CreatedAt >= now {
				now = old.CreatedAt + 1
			}
		}
		c.CreatedAt = now
		c.UpdatedAt = now
		c.ResolvedAt = nil
		if req.Operation == "reply" {
			found := false
			for _, parent := range comments {
				if parent.ID == c.ParentID && parent.ParentID == "" {
					found = true
				}
			}
			if !found {
				return nil, ErrInvalidCommentMutation
			}
		} else {
			c.ParentID = ""
		}
		comments = append(comments, c)
	case "resolve":
		found := false
		for i := range comments {
			if comments[i].ID == req.ID {
				found = true
				now := time.Now().UnixMilli()
				comments[i].UpdatedAt = now
				if req.Resolved {
					comments[i].ResolvedAt = &now
				} else {
					comments[i].ResolvedAt = nil
				}
			}
		}
		if !found {
			return nil, ErrInvalidCommentMutation
		}
	default:
		return nil, ErrInvalidCommentMutation
	}
	return v.writeNoteCommentsLocked(req.Path, comments)
}
