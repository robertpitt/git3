package model

import "encoding/json"

type headAlias Head

// UnmarshalJSON decodes HEAD while preserving unknown top-level fields.
func (h *Head) UnmarshalJSON(b []byte) error {
	var a headAlias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for _, k := range []string{"formatVersion", "requiredFeatures", "repositoryId", "objectFormat", "logicalGeneration", "transactionId", "manifestRevision", "publicationId", "headSymref", "storagePolicy", "refSnapshot", "packset", "log", "gcBarrier"} {
		delete(raw, k)
	}
	*h = Head(a)
	h.Extra = raw
	return nil
}

// MarshalJSON encodes HEAD together with preserved unknown top-level fields.
func (h Head) MarshalJSON() ([]byte, error) {
	x := make(map[string]json.RawMessage, len(h.Extra)+14)
	for k, v := range h.Extra {
		x[k] = v
	}
	a := headAlias(h)
	a.Extra = nil
	b, e := json.Marshal(a)
	if e != nil {
		return nil, e
	}
	var known map[string]json.RawMessage
	if e = json.Unmarshal(b, &known); e != nil {
		return nil, e
	}
	for k, v := range known {
		x[k] = v
	}
	return json.Marshal(x)
}
