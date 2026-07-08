package provider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The Dialog/DialogAnswer wire shapes are a pinned API contract (issue #51
// decision 3): the Go field rename DialogKind→Kind must keep the JSON name
// "dialog_kind", the flat single-question shape must serialize byte-identical
// to the pre-#51 shape (no questions/answers keys — existing compat snapshots
// and SPA parsing stay valid), and the multi-question fields must round-trip.

func TestDialogJSON_flatShapeUnchanged(t *testing.T) {
	d := Dialog{
		ToolID:     "toolu_1",
		Kind:       DialogKindQuestion,
		Prompt:     "Pick one",
		Options:    []DialogOption{{Label: "A"}, {Label: "Other", IsOther: true}},
		Answerable: true,
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"dialog_kind":"question"`) {
		t.Errorf("JSON = %s; want the renamed Kind field still serialized as dialog_kind", s)
	}
	if strings.Contains(s, `"questions"`) {
		t.Errorf("JSON = %s; a single-question dialog must omit the questions key (flat shape)", s)
	}

	var back Dialog
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, d) {
		t.Errorf("round-trip = %+v; want %+v", back, d)
	}
}

func TestDialogJSON_multiQuestionRoundTrip(t *testing.T) {
	d := Dialog{
		ToolID: "toolu_2",
		Kind:   DialogKindQuestion,
		Prompt: "2 questions",
		Questions: []Question{
			{Header: "Scope", Text: "Which scope?", Options: []DialogOption{{Label: "A"}, {Label: "Other", IsOther: true}}},
			{Text: "Which color?", MultiSelect: true, Options: []DialogOption{{Label: "red"}, {Label: "blue"}}},
		},
		Answerable: true,
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"questions"`, `"header":"Scope"`, `"multi_select":true`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON = %s; missing %s", b, want)
		}
	}

	var back Dialog
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, d) {
		t.Errorf("round-trip = %+v; want %+v", back, d)
	}
}

func TestDialogAnswerJSON_perQuestionAnswers(t *testing.T) {
	// Flat shape: no answers key, so pre-#51 clients stay byte-compatible.
	flat, err := json.Marshal(DialogAnswer{Index: 1, OtherText: "free text"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(flat), `"answers"`) {
		t.Errorf("flat answer JSON = %s; must omit the answers key", flat)
	}

	a := DialogAnswer{
		Answers: []QuestionAnswer{
			{Index: 0},
			{Selected: []int{0, 2}},
			{Index: 3, OtherText: "something else"},
		},
	}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var back DialogAnswer
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, a) {
		t.Errorf("round-trip = %+v; want %+v", back, a)
	}
}
