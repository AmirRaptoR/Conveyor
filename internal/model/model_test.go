package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestItemRelationshipsRoundTripAndStayDistinct(t *testing.T) {
	in := Item{
		ID: "s:2", Ref: "2", Source: "s", Stage: "ready", Title: "child",
		Tracking: true, TrackingError: "provider child set incomplete",
		Parent: "s:1", Children: []string{"s:3", "s:4"},
		DependsOn: []string{"s:9"},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var got Item
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Tracking || got.TrackingError != in.TrackingError || got.Parent != in.Parent || strings.Join(got.Children, ",") != "s:3,s:4" ||
		strings.Join(got.DependsOn, ",") != "s:9" {
		t.Fatalf("round trip = %+v, want parent, children and dependency preserved independently", got)
	}

	var old Item
	if err := json.Unmarshal([]byte(`{"id":"s:1","ref":"1","source":"s","stage":"ready","title":"old"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Tracking || old.TrackingError != "" || old.Parent != "" || len(old.Children) != 0 || len(old.DependsOn) != 0 {
		t.Fatalf("old item invented relationships: %+v", old)
	}
}
