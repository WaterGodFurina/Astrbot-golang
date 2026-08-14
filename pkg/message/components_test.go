package message

import "testing"

// TestNodesCloneCarriesForwardIDs verifies Nodes.Clone preserves a deep copy of
// ForwardIDs instead of dropping it (L-46.8).
func TestNodesCloneCarriesForwardIDs(t *testing.T) {
	n := &Nodes{
		Nodes: []*Node{{
			UIN:     "1",
			Name:    "a",
			Content: []Component{&Plain{Text: "x"}},
		}},
		ForwardIDs: []string{"f1", "f2"},
	}
	c := n.Clone().(*Nodes)
	if len(c.ForwardIDs) != 2 || c.ForwardIDs[0] != "f1" || c.ForwardIDs[1] != "f2" {
		t.Fatalf("forward ids not cloned: %v", c.ForwardIDs)
	}
	c.ForwardIDs[0] = "changed"
	if n.ForwardIDs[0] != "f1" {
		t.Fatal("clone shares forward id storage with the original")
	}
	if len(c.Nodes) != 1 || c.Nodes[0].Name != "a" {
		t.Fatalf("nodes not cloned: %v", c.Nodes)
	}
	if _, ok := c.Nodes[0].Content[0].(*Plain); !ok {
		t.Fatal("node content not cloned")
	}
}

// TestJsonCloneDeepCopiesData verifies Json.Clone copies the Data map instead
// of sharing storage with the original (L-46.8).
func TestJsonCloneDeepCopiesData(t *testing.T) {
	inner := map[string]interface{}{"k": "v"}
	j := &Json{Data: map[string]interface{}{
		"a": inner,
		"b": []interface{}{1, 2},
	}}
	c := j.Clone().(*Json)

	c.Data["a"].(map[string]interface{})["k"] = "changed"
	if j.Data["a"].(map[string]interface{})["k"] != "v" {
		t.Fatal("nested map is shared with the clone")
	}
	c.Data["b"].([]interface{})[0] = 99
	if j.Data["b"].([]interface{})[0] != 1 {
		t.Fatal("nested slice is shared with the clone")
	}

	cloneInner := c.Data["a"].(map[string]interface{})
	cloneInner["new"] = "field"
	if _, ok := j.Data["a"].(map[string]interface{})["new"]; ok {
		t.Fatal("added key leaked back into the original")
	}
}

// TestJsonCloneNilData verifies cloning a Json with nil Data does not panic.
func TestJsonCloneNilData(t *testing.T) {
	j := &Json{}
	c := j.Clone().(*Json)
	if c.Data != nil {
		t.Fatalf("expected nil data on clone, got %v", c.Data)
	}
}
