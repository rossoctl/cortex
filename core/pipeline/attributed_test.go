package pipeline

import "testing"

func TestSetParent_FirstCallWins(t *testing.T) {
	c := &Context{}
	c.SetParent("p1", "~/a")
	c.SetParent("p2", "~/b")
	if c.ParentSession() != "p1" || c.ParentProject() != "~/a" {
		t.Errorf("got %q %q, want p1 ~/a", c.ParentSession(), c.ParentProject())
	}

	// A first call with "" pins "": no later call can write a parent.
	c = &Context{}
	c.SetParent("", "")
	c.SetParent("p2", "~/b")
	if c.ParentSession() != "" || c.ParentProject() != "" {
		t.Errorf("got %q %q, want empty", c.ParentSession(), c.ParentProject())
	}
}
