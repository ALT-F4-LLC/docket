#!/usr/bin/env bash
# Section Z: Graph Command

test_z_graph() {
  printf "Section Z: Graph Command\n"

  # Setup: create a small dependency graph for testing.
  # G_A blocks G_B blocks G_C (linear chain).
  run create --json -t "Graph Root"
  assert_exit "Z" "Z0_a" 0
  local G_A
  G_A=$(extract_id)

  run create --json -t "Graph Middle"
  assert_exit "Z" "Z0_b" 0
  local G_B
  G_B=$(extract_id)

  run create --json -t "Graph Leaf"
  assert_exit "Z" "Z0_c" 0
  local G_C
  G_C=$(extract_id)

  run link "$G_A" blocks "$G_B" --json
  assert_exit "Z" "Z0_link1" 0
  run link "$G_B" blocks "$G_C" --json
  assert_exit "Z" "Z0_link2" 0

  # Z1: Basic graph (JSON) — centered on G_B (has both upstream and downstream).
  run graph "DKT-$G_B" --json
  assert_exit "Z" "Z1" 0
  assert_json "Z" "Z1_ok" ".ok" "true"

  # Z2: JSON contract — issue_id, nodes, edges.
  assert_json "Z" "Z2_id" ".data.issue_id" "$G_B"
  assert_json_exists "Z" "Z2_nodes" ".data.nodes"
  assert_json_exists "Z" "Z2_edges" ".data.edges"

  # Z3: Nodes include the focal issue plus connected issues.
  assert_json_array_min "Z" "Z3_nodes" ".data.nodes" 3

  # Z4: Edges exist (A->B and B->C).
  assert_json_array_min "Z" "Z4_edges" ".data.edges" 2

  # Z5: Direction=down — only shows downstream (B blocks C).
  run graph "DKT-$G_B" --json --direction down
  assert_exit "Z" "Z5" 0
  # Should NOT include G_A (upstream of B).
  local HAS_A_DOWN
  HAS_A_DOWN=$(echo "$CMD_STDOUT" | jq "[.data.nodes[] | select(.id == \"DKT-$G_A\")] | length" 2>/dev/null)
  if [ "$HAS_A_DOWN" -eq 0 ]; then
    check "Z" "Z5_no_upstream" "PASS"
  else
    check "Z" "Z5_no_upstream" "FAIL" "upstream issue should not appear with --direction down"
  fi

  # Z6: Direction=up — only shows upstream (A blocks B).
  run graph "DKT-$G_B" --json --direction up
  assert_exit "Z" "Z6" 0
  # Should NOT include G_C (downstream of B).
  local HAS_C_UP
  HAS_C_UP=$(echo "$CMD_STDOUT" | jq "[.data.nodes[] | select(.id == \"DKT-$G_C\")] | length" 2>/dev/null)
  if [ "$HAS_C_UP" -eq 0 ]; then
    check "Z" "Z6_no_downstream" "PASS"
  else
    check "Z" "Z6_no_downstream" "FAIL" "downstream issue should not appear with --direction up"
  fi

  # Z7: Depth limit — depth=1 from B should show A and C but not traverse further.
  run graph "DKT-$G_B" --json --depth 1
  assert_exit "Z" "Z7" 0
  assert_json_array_min "Z" "Z7_nodes" ".data.nodes" 2

  # Z8: Non-existent issue (exit 2).
  run graph "DKT-9999" --json
  assert_exit "Z" "Z8" 2

  # Z9: Invalid issue ID format (exit 3).
  run graph "abc" --json
  assert_exit "Z" "Z9" 3

  # Z10: Invalid direction (exit 3).
  run graph "DKT-$G_B" --json --direction invalid
  assert_exit "Z" "Z10" 3

  # Z11: Negative depth (exit 3).
  run graph "DKT-$G_B" --json --depth -1
  assert_exit "Z" "Z11" 3

  # Z12: Mermaid output contains "graph TD".
  run graph "DKT-$G_B" --mermaid
  assert_exit "Z" "Z12" 0
  assert_stdout_contains "Z" "Z12_header" "graph TD"

  # Z13: Mermaid output contains arrow syntax.
  assert_stdout_contains "Z" "Z13_arrow" "] -->"

  # Z14: Human mode output (tree rendering).
  run graph "DKT-$G_B"
  assert_exit "Z" "Z14" 0
  assert_stdout_contains "Z" "Z14_focal" "DKT-$G_B"

  # Z15: Graph on isolated issue (no relations) — still works.
  run create --json -t "Graph Isolated"
  assert_exit "Z" "Z15_setup" 0
  local G_ISOLATED
  G_ISOLATED=$(extract_id)
  run graph "DKT-$G_ISOLATED" --json
  assert_exit "Z" "Z15" 0
  assert_json "Z" "Z15_ok" ".ok" "true"
  # Only the focal node, no edges.
  assert_json_array_min "Z" "Z15_nodes" ".data.nodes" 1
  local EDGE_COUNT
  EDGE_COUNT=$(echo "$CMD_STDOUT" | jq '.data.edges | length' 2>/dev/null)
  if [ "$EDGE_COUNT" -eq 0 ]; then
    check "Z" "Z15_no_edges" "PASS"
  else
    check "Z" "Z15_no_edges" "FAIL" "isolated issue should have 0 edges, got $EDGE_COUNT"
  fi

  # Z16: DKT- prefix accepted in argument.
  run graph "DKT-$G_B" --json
  assert_exit "Z" "Z16" 0

  # Z17: Numeric ID accepted (without prefix).
  run graph "$G_B" --json
  assert_exit "Z" "Z17" 0
}
