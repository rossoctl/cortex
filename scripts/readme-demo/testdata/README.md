# Replay fixture

`timeline-replay.json.gz` is the live session in the README animation. It is the
shape of one real Claude Code session: a main agent that starts five subagents in
parallel, about 15 minutes long.

`scrub.py` made it from that session's events, as the session API returns them:

```sh
curl -s --noproxy '*' "http://127.0.0.1:47601/v1/sessions/<id>?limit=5000" -o /tmp/raw/s.json
python3 -I scripts/readme-demo/testdata/scrub.py /tmp/raw/s.json scripts/readme-demo/testdata/timeline-replay.json.gz
```

What it keeps: times, durations, token counts, models, finish reasons, tool names and
ids, the cost record, and the markers that link a subagent to its parent
(`agentId:`, `<task-notification>`, `<agent-message>`). What it replaces:

- every message, tool result and completion becomes a hash, so a retry still reads as
  a retry;
- the subagent labels, commands and paths become invented ones;
- the model gateway becomes `api.anthropic.com`, and hosts other than it and GitHub
  are dropped;
- each request keeps only the messages new since its agent's last request, and each
  tool result is kept once per agent.

It fails if your home directory, your user name or the gateway host survives. Keep the
raw file out of the repo, and delete it when you are done.
