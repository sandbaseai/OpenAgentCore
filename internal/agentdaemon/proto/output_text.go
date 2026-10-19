package proto

import (
	"bytes"
	"encoding/json"
	"strings"
)

// NormalizeOutputText renders NUL in command deltas and tool output values as
// printable text. Arguments, identifiers and metadata are not output text.
// Both Runtime producers and Core journal ingestion use this boundary rule.
func NormalizeOutputText(env Envelope) (Envelope, error) {
	if env.Type != TypeCommandOutput && env.Type != TypeToolCall {
		return env, nil
	}
	if !bytes.Contains(env.Payload, []byte(`\u0000`)) {
		return env, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(env.Payload, &fields); err != nil {
		return env, err
	}
	changed := false
	normalize := func(fields map[string]json.RawMessage, key string) error {
		raw, ok := fields[key]
		if !ok {
			return nil
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		value, modified := normalizeOutputValue(value)
		if !modified {
			return nil
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		fields[key] = encoded
		changed = true
		return nil
	}
	if env.Type == TypeCommandOutput {
		if err := normalize(fields, "delta"); err != nil {
			return env, err
		}
	} else {
		if err := normalize(fields, "result"); err != nil {
			return env, err
		}
		if raw, ok := fields["observation"]; ok && string(raw) != "null" {
			var observation map[string]json.RawMessage
			if err := json.Unmarshal(raw, &observation); err != nil {
				return env, err
			}
			if err := normalize(observation, "output"); err != nil {
				return env, err
			}
			if changed {
				encoded, err := json.Marshal(observation)
				if err != nil {
					return env, err
				}
				fields["observation"] = encoded
			}
		}
	}
	if !changed {
		return env, nil
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return env, err
	}
	env.Payload = raw
	return env, nil
}

func normalizeOutputValue(value any) (any, bool) {
	switch v := value.(type) {
	case string:
		if strings.ContainsRune(v, 0) {
			return strings.ReplaceAll(v, "\x00", `\x00`), true
		}
	case []any:
		changed := false
		for i, item := range v {
			next, modified := normalizeOutputValue(item)
			v[i] = next
			changed = changed || modified
		}
		return v, changed
	case map[string]any:
		changed := false
		for key, item := range v {
			next, modified := normalizeOutputValue(item)
			v[key] = next
			changed = changed || modified
		}
		return v, changed
	}
	return value, false
}
