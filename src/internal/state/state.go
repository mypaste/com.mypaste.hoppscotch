// Package state validates the one JSON document the workspace page saves.
//
// The page owns the document's shape; this package owns the boundary. It
// refuses anything that is not a small, valid JSON object and checks the
// three arrays the page relies on, so a corrupt file is reported instead of
// being loaded half-way or written back as something else.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// MaxBytes is the most one saved workspace may carry. The host's invoke line
// is capped at 1 MiB, and the page sends the document inside it.
const MaxBytes = 512 << 10

// MaxItems bounds the three top-level arrays so one bad page cannot grow a
// workspace without a ceiling.
const MaxItems = 2000

// Validate accepts the page's saved document or explains why it cannot.
func Validate(raw []byte) error {
	if len(raw) == 0 {
		return errors.New("工作区为空")
	}
	if len(raw) > MaxBytes {
		return fmt.Errorf("工作区超过 %d KiB", MaxBytes>>10)
	}
	if !utf8.Valid(raw) {
		return errors.New("工作区不是有效 UTF-8")
	}
	if !json.Valid(raw) {
		return errors.New("工作区不是有效 JSON")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return errors.New("工作区必须是 JSON 对象")
	}
	for _, key := range []string{"collections", "history", "environments"} {
		value, ok := top[key]
		if !ok {
			continue
		}
		var items []json.RawMessage
		if err := json.Unmarshal(value, &items); err != nil {
			return fmt.Errorf("工作区的 %s 必须是数组", key)
		}
		if len(items) > MaxItems {
			return fmt.Errorf("工作区的 %s 超过 %d 项", key, MaxItems)
		}
	}
	return nil
}
