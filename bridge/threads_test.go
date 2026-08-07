package bridge_test

import (
	"testing"
	"time"

	"github.com/wevial/croton-mcp/bridge"
)

func TestBuildThreadLinksReferencesInDeterministicDateOrder(t *testing.T) {
	t.Parallel()

	thread := bridge.BuildThread([]bridge.ThreadMessage{
		{
			Key: "reply",
			Headers: bridge.CanonicalHeaders{
				MessageID:  "<reply@fixture.test>",
				InReplyTo:  "<root@fixture.test>",
				References: []string{"<root@fixture.test>"},
			},
			ReceivedAt: time.Date(2026, time.August, 6, 12, 1, 0, 0, time.UTC),
		},
		{
			Key: "root",
			Headers: bridge.CanonicalHeaders{
				MessageID: "<root@fixture.test>",
			},
			ReceivedAt: time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC),
		},
	}, bridge.ThreadOptions{})

	if len(thread.Nodes) != 2 {
		t.Fatalf("node count = %d, want 2", len(thread.Nodes))
	}

	if thread.Nodes[0].Key != "root" || thread.Nodes[0].Depth != 0 || thread.Nodes[0].ParentKey != "" {
		t.Errorf("root node = %+v", thread.Nodes[0])
	}

	if thread.Nodes[1].Key != "reply" || thread.Nodes[1].Depth != 1 || thread.Nodes[1].ParentKey != "root" {
		t.Errorf("reply node = %+v", thread.Nodes[1])
	}

	if thread.Truncation.Any() {
		t.Errorf("unexpected truncation: %+v", thread.Truncation)
	}
}

func TestBuildThreadRecordsMissingParentsAndBreaksCycles(t *testing.T) {
	t.Parallel()

	thread := bridge.BuildThread([]bridge.ThreadMessage{
		{
			Key:        "a",
			Headers:    bridge.CanonicalHeaders{MessageID: "<a@fixture.test>", References: []string{"<b@fixture.test>"}},
			ReceivedAt: time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC),
		},
		{
			Key:        "b",
			Headers:    bridge.CanonicalHeaders{MessageID: "<b@fixture.test>", References: []string{"<a@fixture.test>"}},
			ReceivedAt: time.Date(2026, time.August, 6, 12, 1, 0, 0, time.UTC),
		},
		{
			Key:        "orphan",
			Headers:    bridge.CanonicalHeaders{MessageID: "<orphan@fixture.test>", InReplyTo: "<missing@fixture.test>"},
			ReceivedAt: time.Date(2026, time.August, 6, 12, 2, 0, 0, time.UTC),
		},
	}, bridge.ThreadOptions{})

	if !thread.Truncation.ThreadCycle {
		t.Errorf("truncation = %+v, want cycle metadata", thread.Truncation)
	}

	if thread.Nodes[1].ParentKey != "" || thread.Nodes[2].MissingParentID != "<missing@fixture.test>" {
		t.Errorf("cycle/missing-parent resolution = %+v", thread.Nodes)
	}
}

func TestBuildThreadCapsDepthAndMessageCount(t *testing.T) {
	t.Parallel()

	thread := bridge.BuildThread([]bridge.ThreadMessage{
		{Key: "one", Headers: bridge.CanonicalHeaders{MessageID: "<one@fixture.test>"}, ReceivedAt: time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)},
		{Key: "two", Headers: bridge.CanonicalHeaders{MessageID: "<two@fixture.test>", InReplyTo: "<one@fixture.test>"}, ReceivedAt: time.Date(2026, time.August, 6, 12, 1, 0, 0, time.UTC)},
		{Key: "three", Headers: bridge.CanonicalHeaders{MessageID: "<three@fixture.test>", InReplyTo: "<two@fixture.test>"}, ReceivedAt: time.Date(2026, time.August, 6, 12, 2, 0, 0, time.UTC)},
	}, bridge.ThreadOptions{Limits: bridge.NormalizeLimits{MaxThreadDepth: 1}})

	if !thread.Truncation.ThreadDepth || thread.Nodes[2].ParentKey != "" {
		t.Errorf("depth cap result = %+v, truncation = %+v", thread.Nodes, thread.Truncation)
	}

	countCapped := bridge.BuildThread([]bridge.ThreadMessage{
		{Key: "z", Headers: bridge.CanonicalHeaders{MessageID: "<z@fixture.test>"}, ReceivedAt: time.Date(2026, time.August, 6, 12, 2, 0, 0, time.UTC)},
		{Key: "a", Headers: bridge.CanonicalHeaders{MessageID: "<a@fixture.test>"}, ReceivedAt: time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)},
		{Key: "m", Headers: bridge.CanonicalHeaders{MessageID: "<m@fixture.test>"}, ReceivedAt: time.Date(2026, time.August, 6, 12, 1, 0, 0, time.UTC)},
	}, bridge.ThreadOptions{Limits: bridge.NormalizeLimits{MaxThreadMessages: 2}})

	if !countCapped.Truncation.ThreadMessages || len(countCapped.Nodes) != 2 || countCapped.Nodes[0].Key != "a" || countCapped.Nodes[1].Key != "m" {
		t.Errorf("count cap result = %+v, truncation = %+v", countCapped.Nodes, countCapped.Truncation)
	}
}

func TestBuildThreadUsesFirstDateOrderedDuplicateMessageID(t *testing.T) {
	t.Parallel()

	thread := bridge.BuildThread([]bridge.ThreadMessage{
		{Key: "later", Headers: bridge.CanonicalHeaders{MessageID: "<duplicate@fixture.test>"}, ReceivedAt: time.Date(2026, time.August, 6, 12, 1, 0, 0, time.UTC)},
		{Key: "first", Headers: bridge.CanonicalHeaders{MessageID: "<duplicate@fixture.test>"}, ReceivedAt: time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)},
		{Key: "reply", Headers: bridge.CanonicalHeaders{MessageID: "<reply@fixture.test>", InReplyTo: "<duplicate@fixture.test>"}, ReceivedAt: time.Date(2026, time.August, 6, 12, 2, 0, 0, time.UTC)},
	}, bridge.ThreadOptions{})

	if thread.Nodes[2].ParentKey != "first" {
		t.Errorf("duplicate-ID parent = %q, want first date-ordered occurrence", thread.Nodes[2].ParentKey)
	}
}

func TestBuildThreadKeepsNearestReferencesWhenCapped(t *testing.T) {
	t.Parallel()

	thread := bridge.BuildThread([]bridge.ThreadMessage{
		{Key: "root", Headers: bridge.CanonicalHeaders{MessageID: "<root@fixture.test>"}, ReceivedAt: time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)},
		{Key: "reply", Headers: bridge.CanonicalHeaders{MessageID: "<reply@fixture.test>", References: []string{"<missing@fixture.test>", "<root@fixture.test>"}}, ReceivedAt: time.Date(2026, time.August, 6, 12, 1, 0, 0, time.UTC)},
	}, bridge.ThreadOptions{Limits: bridge.NormalizeLimits{MaxReferenceCount: 1}})

	if !thread.Truncation.References || thread.Nodes[1].ParentKey != "root" {
		t.Errorf("reference cap result = %+v, truncation = %+v", thread.Nodes, thread.Truncation)
	}
}
