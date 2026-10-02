// Draft frozen-contract witness: public CLI MCP against synthetic loopback TLS.
// The shared triage harness owns config/process isolation and bounded fault waits.
package mcpserver_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestStoryTriageSpecial(t *testing.T) {
	enabled := map[string]any{"enabled": true}
	t.Run("default_off_public_cli_catalog_and_calls", func(t *testing.T) {
		for _, config := range []struct {
			name  string
			value any
		}{{"missing", nil}, {"explicit_false", map[string]any{"enabled": false}}} {
			t.Run(config.name, func(t *testing.T) {
				h := triageStart(t, triageSeeds(), config.value)
				before := h.fixture.Snapshot()
				catalog, err := h.session.ListTools(h.ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				triageCatalog(t, catalog)
				for _, tool := range catalog.Tools {
					if tool.Name == "archive_mail" || tool.Name == "trash_mail" {
						t.Fatalf("default-off CLI exposed %s", tool.Name)
					}
				}
				for _, name := range []string{"archive_mail", "trash_mail"} {
					result, err := h.session.CallTool(h.ctx, &mcp.CallToolParams{Name: name, Arguments: triageArgs([]uint32{101})})
					if err == nil && (result == nil || !result.IsError) {
						t.Fatalf("default-off CLI accepted %s", name)
					}
				}
				triageNoWrites(t, h, before)
			})
		}
	})

	t.Run("explicit_opt_in_public_cli_catalog", func(t *testing.T) {
		h := triageStart(t, triageSeeds(), enabled)
		h.requireTools(t, "archive_mail", "trash_mail", "move_mail", "list_folders", "search_mail")
		catalog, err := h.session.ListTools(h.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range catalog.Tools {
			if tool.Name != "archive_mail" && tool.Name != "trash_mail" {
				continue
			}
			schema, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			var object struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(schema, &object); err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"approved", "destination", "ordinal", "ordinals"} {
				if _, ok := object.Properties[forbidden]; ok {
					t.Fatalf("%s schema exposes %s", tool.Name, forbidden)
				}
			}
		}
	})

	for _, operation := range []struct{ tool, destination, attribute string }{
		{"archive_mail", "Archivage", `\Archive`},
		{"trash_mail", "Corbeille", `\Trash`},
	} {
		t.Run(operation.tool, func(t *testing.T) {
			t.Run("ordinary_list_unique_opaque_target_first_three_of_ten", func(t *testing.T) {
				seeds := triageSeeds()
				// Guarantee the later unread presentation differs, independently of
				// shared seed flag choices; moving must preserve these flags too.
				for i := range seeds.Mailboxes {
					if seeds.Mailboxes[i].Name != "INBOX" {
						continue
					}
					for j := range seeds.Mailboxes[i].Messages {
						if seeds.Mailboxes[i].Messages[j].UID == 101 {
							seeds.Mailboxes[i].Messages[j].Flags = []string{`\Seen`, `\Flagged`}
						}
					}
				}
				h := triageStart(t, seeds, enabled)
				h.requireTools(t, operation.tool, "search_mail")
				before := h.fixture.Snapshot()
				// Capture all ten displayed identities through the public read surface.
				captured := triageDisplayed(t, h, false)
				displayed := h.call(t, "search_mail", map[string]any{"mailbox": "INBOX", "limit": 10})
				triageSpecialDisplayed(t, displayed, 10)
				// A later search reverses the presentation locally and omits seen IDs.
				// The original UID snapshot, not either presentation's ordinals, is used.
				later := h.call(t, "search_mail", map[string]any{"mailbox": "INBOX", "unreadOnly": true, "limit": 10})
				triageSpecialReordered(t, displayed, later)
				uids := append([]uint32(nil), captured[:3]...)
				cut := triageSpecialCut(t, h)
				result := h.call(t, operation.tool, triageArgs(uids))
				triageResults(t, result, uids, []string{"applied", "applied", "applied"}, "")
				triageMoved(t, before, h.fixture.Snapshot(), "INBOX", operation.destination, uids)
				triageSpecialMoves(t, h, cut, operation.destination, uids)
			})

			t.Run("unsupported_mapping_zero_duplicate_nonselectable_and_move_absent", func(t *testing.T) {
				for _, scenario := range []string{"zero", "duplicate", "noselect", "nonexistent", "move_absent", "display_name_only"} {
					t.Run(scenario, func(t *testing.T) {
						seeds := triageSeeds()
						for i := range seeds.Mailboxes {
							if seeds.Mailboxes[i].Name != operation.destination {
								continue
							}
							switch scenario {
							case "zero", "display_name_only":
								seeds.Mailboxes[i].Attributes = nil
							case "noselect":
								seeds.Mailboxes[i].Attributes = []string{operation.attribute, `\Noselect`}
							case "nonexistent":
								seeds.Mailboxes[i].Attributes = []string{operation.attribute, `\NonExistent`}
							}
						}
						if scenario == "duplicate" {
							seeds.Mailboxes = append(seeds.Mailboxes, testkit.MailboxSeed{Name: "Folders/Second", Attributes: []string{operation.attribute}})
						}
						if scenario == "display_name_only" {
							seeds.Mailboxes = append(seeds.Mailboxes, testkit.MailboxSeed{Name: "Archive"}, testkit.MailboxSeed{Name: "Trash"})
						}
						if scenario == "move_absent" {
							seeds.Move = false
						}
						h := triageStart(t, seeds, enabled)
						h.requireTools(t, operation.tool)
						before := h.fixture.Snapshot()
						uids := []uint32{101, 102}
						triageResults(t, h.call(t, operation.tool, triageArgs(uids)), uids, []string{"refused", "refused"}, "unsupported")
						triageNoWrites(t, h, before)
						triageSpecialOrdinaryLists(t, h)
					})
				}
			})

			t.Run("folders_only_attribute_does_not_authorize_labels_or_virtual_views", func(t *testing.T) {
				for _, destination := range []string{"Labels/Tag", "All Mail", "Starred"} {
					t.Run(strings.ReplaceAll(destination, "/", "_"), func(t *testing.T) {
						seeds := triageSeeds()
						for i := range seeds.Mailboxes {
							if seeds.Mailboxes[i].Name == operation.destination {
								seeds.Mailboxes[i].Attributes = nil
							}
							if seeds.Mailboxes[i].Name == destination {
								seeds.Mailboxes[i].Attributes = []string{operation.attribute}
							}
						}
						h := triageStart(t, seeds, enabled)
						h.requireTools(t, operation.tool)
						before := h.fixture.Snapshot()
						triageResults(t, h.call(t, operation.tool, triageArgs([]uint32{101})), []uint32{101}, []string{"refused"}, "unsupported")
						triageNoWrites(t, h, before)
					})
				}
			})

			t.Run("fresh_same_connection_mapping_after_warmed_list_attribute_change", func(t *testing.T) {
				h := triageStart(t, triageSeeds(), enabled)
				h.requireTools(t, operation.tool, "list_folders")
				triageSpecialReadOK(t, h.call(t, "list_folders", map[string]any{}))
				// First move warms both public folder discovery and mutation discovery.
				before := h.fixture.Snapshot()
				cut := triageSpecialCut(t, h)
				triageResults(t, h.call(t, operation.tool, triageArgs([]uint32{101})), []uint32{101}, []string{"applied"}, "")
				triageMoved(t, before, h.fixture.Snapshot(), "INBOX", operation.destination, []uint32{101})
				triageSpecialMoves(t, h, cut, operation.destination, []uint32{101})
				if err := h.fixture.SetMailboxAttributes(operation.destination); err != nil {
					t.Fatal(err)
				}
				if err := h.fixture.SetMailboxAttributes("Folders/Existing", operation.attribute); err != nil {
					t.Fatal(err)
				}
				before = h.fixture.Snapshot()
				cut = triageSpecialCut(t, h)
				triageResults(t, h.call(t, operation.tool, triageArgs([]uint32{102, 103})), []uint32{102, 103}, []string{"applied", "applied"}, "")
				triageMoved(t, before, h.fixture.Snapshot(), "INBOX", "Folders/Existing", []uint32{102, 103})
				triageSpecialMoves(t, h, cut, "Folders/Existing", []uint32{102, 103})
			})

			t.Run("fresh_mapping_after_warmed_list_removed_or_invalidated_target", func(t *testing.T) {
				for _, change := range []string{"removed", "attribute_removed", "duplicate_added", "nonselectable"} {
					t.Run(change, func(t *testing.T) {
						h := triageStart(t, triageSeeds(), enabled)
						h.requireTools(t, operation.tool, "list_folders")
						triageSpecialReadOK(t, h.call(t, "list_folders", map[string]any{}))
						var err error
						switch change {
						case "removed":
							err = h.fixture.RemoveMailbox(operation.destination)
						case "attribute_removed":
							err = h.fixture.SetMailboxAttributes(operation.destination)
						case "duplicate_added":
							err = h.fixture.SetMailboxAttributes("Folders/Existing", operation.attribute)
						case "nonselectable":
							err = h.fixture.SetMailboxAttributes(operation.destination, operation.attribute, `\Noselect`)
						}
						if err != nil {
							t.Fatal(err)
						}
						before := h.fixture.Snapshot()
						cut := triageSpecialCut(t, h)
						triageResults(t, h.call(t, operation.tool, triageArgs([]uint32{102})), []uint32{102}, []string{"refused"}, "unsupported")
						triageNoWrites(t, h, before)
						triageSpecialFreshList(t, h, cut)
					})
				}
			})

			t.Run("same_source_mapped_destination_per_id_refused_without_move", func(t *testing.T) {
				seeds := triageSeeds()
				for i := range seeds.Mailboxes {
					if seeds.Mailboxes[i].Name == operation.destination {
						seeds.Mailboxes[i].Attributes = nil
					}
					if seeds.Mailboxes[i].Name == "INBOX" {
						seeds.Mailboxes[i].Attributes = []string{operation.attribute}
					}
				}
				h := triageStart(t, seeds, enabled)
				h.requireTools(t, operation.tool)
				before := h.fixture.Snapshot()
				result := h.call(t, operation.tool, triageArgs([]uint32{101, 102}))
				// The frozen contract specifies refused but does not name its code.
				code := "" // code is optional in the frozen envelope
				triageResults(t, result, []uint32{101, 102}, []string{"refused", "refused"}, code)
				triageNoWrites(t, h, before)
			})

			t.Run("changed_source_generation_after_display", func(t *testing.T) {
				h := triageStart(t, triageSeeds(), enabled)
				h.requireTools(t, operation.tool)
				_ = triageDisplayed(t, h, false)
				if err := h.fixture.ReplaceUIDValidity("INBOX", 7999); err != nil {
					t.Fatal(err)
				}
				before := h.fixture.Snapshot()
				r, err := h.session.CallTool(h.ctx, &mcp.CallToolParams{Name: operation.tool, Arguments: triageArgs([]uint32{101})})
				if err == nil && r != nil && !r.IsError {
					triageResults(t, r, []uint32{101}, []string{"refused"}, "")
				}
				triageNoWrites(t, h, before)
			})
			t.Run("malformed_identity_stale_generation_and_source_preflight", func(t *testing.T) {
				triageBadInputs(t, operation.tool, "")
			})
			t.Run("mixed_outcomes_lost_completion_and_timeouts_no_replay", func(t *testing.T) {
				for _, boundary := range []testkit.FaultBoundary{testkit.BeforeApplication, testkit.AfterApplication} {
					for _, action := range []struct {
						name  string
						value testkit.FaultAction
					}{{"lost_completion", testkit.DropConnection}, {"timeout", testkit.HoldResponse}} {
						t.Run(boundary.String()+"_"+action.name, func(t *testing.T) {
							h := triageStart(t, triageSeeds(), enabled)
							h.requireTools(t, operation.tool)
							triageFault(t, h, operation.tool, operation.destination, "UID MOVE", boundary, action.value)
							triageSpecialOrdinaryLists(t, h)
						})
					}
				}
			})
		})
	}

	// Existing six-read regression tests/guards are retained untouched. The
	// stateful fixture deliberately does not implement BODYSTRUCTURE/ENVELOPE;
	// those original legacy-fixture tests must remain the six-read evidence.

	t.Run("root-system-archive-trash-ordinary-list", func(t *testing.T) {
		for _, operation := range []struct{ tool, old, destination string }{{"archive_mail", "Archivage", "Archive"}, {"trash_mail", "Corbeille", "Trash"}} {
			t.Run(operation.tool, func(t *testing.T) {
				seeds := triageSeeds()
				for i := range seeds.Mailboxes {
					if seeds.Mailboxes[i].Name == operation.old {
						seeds.Mailboxes[i].Name = operation.destination
					}
				}
				h := triageStart(t, seeds, enabled)
				h.requireTools(t, operation.tool)
				before := h.fixture.Snapshot()
				cut := triageSpecialCut(t, h)
				uids := []uint32{101, 103}
				triageResults(t, h.call(t, operation.tool, triageArgs(uids)), uids, []string{"applied", "applied"}, "")
				triageMoved(t, before, h.fixture.Snapshot(), "INBOX", operation.destination, uids)
				triageSpecialMoves(t, h, cut, operation.destination, uids)
			})
		}
	})
	t.Run("explicit_existing_folder_independent_of_archive_trash_mapping", func(t *testing.T) {
		seeds := triageSeeds()
		for i := range seeds.Mailboxes {
			if seeds.Mailboxes[i].Name == "Archivage" || seeds.Mailboxes[i].Name == "Corbeille" {
				seeds.Mailboxes[i].Attributes = nil
			}
		}
		h := triageStart(t, seeds, enabled)
		h.requireTools(t, "move_mail")
		before := h.fixture.Snapshot()
		uids := []uint32{101, 103}
		args := triageArgs(uids)
		args["destination"] = "Folders/Existing"
		cut := triageSpecialCut(t, h)
		triageResults(t, h.call(t, "move_mail", args), uids, []string{"applied", "applied"}, "")
		triageMoved(t, before, h.fixture.Snapshot(), "INBOX", "Folders/Existing", uids)
		triageSpecialMoves(t, h, cut, "Folders/Existing", uids)
	})
}

func triageSpecialCut(t *testing.T, h *triageHarness) int {
	t.Helper()
	commands := triageCommands(t, h)
	if len(commands) == 0 {
		return 0
	}
	return commands[len(commands)-1].Sequence
}

func triageSpecialOrdinaryLists(t *testing.T, h *triageHarness) {
	t.Helper()
	for _, command := range triageCommands(t, h) {
		if command.Verb == "LIST" && command.Arguments != `"" "*"` && command.Arguments != `"" *` {
			t.Fatalf("LIST must be ordinary full namespace discovery, sequence=%d", command.Sequence)
		}
		if command.Verb == "UID STORE" {
			t.Fatalf("archive/trash changed flags, sequence=%d", command.Sequence)
		}
	}
}

func triageSpecialFreshList(t *testing.T, h *triageHarness, cut int) {
	t.Helper()
	triageSpecialOrdinaryLists(t, h)
	for _, command := range triageCommands(t, h) {
		if command.Sequence > cut && command.Verb == "LIST" {
			return
		}
	}
	t.Fatal("no fresh ordinary LIST after warmed discovery was invalidated")
}

func triageSpecialMoves(t *testing.T, h *triageHarness, cut int, destination string, uids []uint32) {
	t.Helper()
	triageSpecialOrdinaryLists(t, h)
	commands := triageCommands(t, h)
	var moves []testkit.TranscriptCommand
	for _, command := range triageDispatches(t, h, "UID MOVE") {
		if command.Sequence > cut {
			moves = append(moves, command)
		}
	}
	if len(moves) != len(uids) {
		t.Fatalf("MOVE dispatches=%d want exactly %d (one per ID, no replay)", len(moves), len(uids))
	}
	previous := cut
	for i, move := range moves {
		if !move.TLS || move.Destination != destination || move.Set != fmt.Sprint(uids[i]) {
			t.Fatalf("MOVE %d differs from exact TLS UID/destination request: %+v", i, move)
		}
		fresh := false
		for _, command := range commands {
			if command.Sequence > previous && command.Sequence < move.Sequence && command.ConnectionID == move.ConnectionID && command.Verb == "LIST" && command.TLS {
				fresh = true
			}
		}
		if !fresh {
			t.Fatalf("UID %d lacks fresh ordinary LIST on its MOVE connection", uids[i])
		}
		previous = move.Sequence
	}
}

func triageSpecialReadOK(t *testing.T, result *mcp.CallToolResult) {
	t.Helper()
	if result == nil || result.IsError {
		t.Fatalf("public read tool failed: %+v", result)
	}
}

func triageSpecialDisplayed(t *testing.T, result *mcp.CallToolResult, count int) []string {
	t.Helper()
	triageSpecialReadOK(t, result)
	data := triageJSON(t, result)
	var value struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if count >= 0 && len(value.Results) != count {
		t.Fatalf("displayed identities=%d want %d", len(value.Results), count)
	}
	ids := make([]string, len(value.Results))
	seen := map[string]bool{}
	for i, item := range value.Results {
		if item.ID == "" || seen[item.ID] {
			t.Fatal("displayed identities empty or duplicated")
		}
		ids[i], seen[item.ID] = item.ID, true
	}
	return ids
}

func triageSpecialReordered(t *testing.T, original, later *mcp.CallToolResult) {
	t.Helper()
	ids := triageSpecialDisplayed(t, original, 10)
	subset := triageSpecialDisplayed(t, later, -1)
	if len(subset) == 0 || len(subset) >= len(ids) {
		t.Fatal("later unread search must change the displayed selection")
	}
	// A client may sort its display differently without changing server state.
	for i, j := 0, len(subset)-1; i < j; i, j = i+1, j-1 {
		subset[i], subset[j] = subset[j], subset[i]
	}
	if subset[0] == ids[0] {
		t.Fatal("later presentation must have a different first identity")
	}
}
