package pipeline

// SetAttribution pins the process and the naming header the listener's attribution gave this
// request, for every row recorded from it. FIRST CALL WINS, on ResolveClient's terms: the
// listener sets it before the pipeline runs, so no plugin can re-attribute a row, and a later
// re-resolution on a recording path cannot disagree with the first.
//
// proc is attribution.Attribution.Proc, header the session-id header that named the session
// ("" when none did).
func (c *Context) SetAttribution(proc, clientProc, header string) {
	if c.attributed {
		return
	}
	c.attributed = true
	c.attributedProc, c.clientProc, c.namingHeader = proc, clientProc, header
}

// AttributedClientProc is the pinned client process when it differs from AttributedProc (a
// worker under its launcher), "" otherwise. Recorded as SessionEvent.ClientProc.
func (c *Context) AttributedClientProc() string { return c.clientProc }

// AttributedProc is the pinned process, "" when the listener could not name it or pinned
// nothing. Recorded as SessionEvent.Proc.
func (c *Context) AttributedProc() string { return c.attributedProc }

// NamingHeader is the pinned session-id header that named the request's session, "" when
// none did. Recorded as SessionEvent.SessionHeader on the request's own row.
func (c *Context) NamingHeader() string { return c.namingHeader }
