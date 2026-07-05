package main

import (
	"reflect"
	"testing"
)

func TestPickLowestIssue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		issues []Issue
		want   Issue
		ok     bool
	}{
		{"empty queue", nil, Issue{}, false},
		{"single", []Issue{{Index: 7, Title: "a"}}, Issue{Index: 7, Title: "a"}, true},
		{"lowest regardless of order", []Issue{{Index: 9}, {Index: 3}, {Index: 12}, {Index: 5}}, Issue{Index: 3}, true},
		{"already ascending", []Issue{{Index: 2, Title: "x"}, {Index: 4}}, Issue{Index: 2, Title: "x"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pickLowestIssue(tc.issues)
			if ok != tc.ok || got != tc.want {
				t.Errorf("pickLowestIssue(%+v) = (%+v,%v); want (%+v,%v)", tc.issues, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// tea renders the issue number as a quoted string ("index":"62"); parseIssues
// must convert it and skip any entry whose index isn't a number, rather than
// failing the whole list.
func TestParseIssues_stringIndexAndSkips(t *testing.T) {
	data := []byte(`[
		{"index":"62","title":"a","state":"open","labels":"ready-for-agent"},
		{"index":"7","title":"b"},
		{"index":"notanumber","title":"skip me"}
	]`)
	got, err := parseIssues(data)
	if err != nil {
		t.Fatalf("parseIssues: %v", err)
	}
	want := []Issue{{Index: 62, Title: "a"}, {Index: 7, Title: "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseIssues = %+v; want %+v", got, want)
	}
	if low, ok := pickLowestIssue(got); !ok || low.Index != 7 {
		t.Errorf("pickLowestIssue(decoded) = (%+v,%v); want index 7", low, ok)
	}
}

func TestParseIssues_empty(t *testing.T) {
	got, err := parseIssues([]byte(`[]`))
	if err != nil {
		t.Fatalf("parseIssues([]): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parseIssues([]) = %+v; want empty", got)
	}
}

// parsePulls reads only head + state from `tea pulls list --output json`; tea
// renders state as "open"/"merged"/"closed" and head as the bare branch name.
func TestParsePulls_headAndState(t *testing.T) {
	data := []byte(`[
		{"index":"72","head":"afk/63","state":"merged"},
		{"index":"40","head":"afk/12","state":"open"},
		{"index":"9","head":"afk/7","state":"closed"}
	]`)
	got, err := parsePulls(data)
	if err != nil {
		t.Fatalf("parsePulls: %v", err)
	}
	want := []PullRequest{
		{Head: "afk/63", State: pullMerged},
		{Head: "afk/12", State: pullOpen},
		{Head: "afk/7", State: pullClosed},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsePulls = %+v; want %+v", got, want)
	}
}

func TestParsePulls_empty(t *testing.T) {
	got, err := parsePulls([]byte(`[]`))
	if err != nil {
		t.Fatalf("parsePulls([]): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parsePulls([]) = %+v; want empty", got)
	}
}
