package input

import "testing"

func TestKeyboardInterception(t *testing.T) {
	var k keyboardModel
	k.key('A', true)
	k.setPolicy(KeyboardPolicy{TargetProcessID: 123, Block: []KeyChord{{'A', 0}, {'R', 3}, {186, 0}}, PassThrough: []KeyChord{{186, 0}}})
	k.context(true)
	if _, block := k.key('A', true); block {
		t.Fatal("grabbed key held before editing")
	}
	k.key('A', false)
	if changed, block := k.key('A', true); !changed || !block {
		t.Fatal("new edit press not blocked")
	}
	if changed, block := k.key('A', true); changed || !block {
		t.Fatal("repeat must be suppressed without another event")
	}
	if _, block := k.key('A', false); !block {
		t.Fatal("blocked release missing")
	}
	if k.status.BlockedEdges != 2 {
		t.Fatal("repeat inflated edge count")
	}
	if _, block := k.key(186, true); block {
		t.Fatal("recenter pass-through must win")
	}
	k.key(186, false)
	k.key(162, true)
	k.key(164, true)
	if _, block := k.key('R', true); !block {
		t.Fatal("chord not blocked")
	}
	k.key(162, false)
	k.key(164, false)
	if _, block := k.key('R', false); !block {
		t.Fatal("release lost when modifiers released first")
	}
	k.key('A', true)
	k.context(false)
	if _, block := k.key('A', true); block {
		t.Fatal("focus loss must fail open")
	}
	k.context(true)
	if _, block := k.key('A', true); block {
		t.Fatal("held on reentry must pass")
	}
	k.key('A', false)
	k.key('A', true)
	k.setPolicy(KeyboardPolicy{})
	if _, block := k.key('A', false); block {
		t.Fatal("disabled still intercepts")
	}
}
func TestKeyboardPolicyValidationAndCopy(t *testing.T) {
	for _, policy := range []KeyboardPolicy{
		{Block: []KeyChord{{'A', 0}}}, {TargetProcessID: 1, Block: []KeyChord{{162, 0}}},
		{TargetProcessID: 1, Block: []KeyChord{{'A', 16}}}, {TargetProcessID: 1, Block: []KeyChord{{'A', 0}, {'A', 0}}},
	} {
		if policy.validate() == nil {
			t.Fatalf("invalid policy accepted: %+v", policy)
		}
	}
	p := KeyboardPolicy{TargetProcessID: 1, Block: []KeyChord{{'A', 0}}, PassThrough: []KeyChord{{'A', 0}}}
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	copy := p.clone()
	p.Block[0].Code = 'B'
	if copy.Block[0].Code != 'A' {
		t.Fatal("policy aliases caller memory")
	}
}
