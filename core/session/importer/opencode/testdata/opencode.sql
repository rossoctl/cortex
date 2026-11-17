-- A scrubbed opencode.db: only the columns that the OpenCode source reads.
-- Times are milliseconds since the epoch. Group times (the newest event time of a group):
-- ses_tools 1763000009000 (from its subagent ses_child), ses_plain 1763000006000 (from its
-- fork ses_fork), ses_gone 1763000000200 (ses_orphan, whose parent row does not exist).
-- ses_fork holds copies of the ses_plain rows, with new ids and the original times.
create table session_v2 (
  id text primary key,
  parent_id text,
  fork_session_id text,
  directory text not null,
  version text not null,
  time_created integer not null
);
create table session_message (
  id text primary key,
  session_id text not null,
  type text not null,
  seq integer not null,
  time_created integer not null,
  time_updated integer not null,
  data text not null,
  unique (session_id, seq)
);

insert into session_v2 values ('ses_plain', null, null, '/tmp/demo', '2.0.26', 1763000000500);
insert into session_message values ('msg_p1', 'ses_plain', 'user', 1, 1763000001000, 1763000001000,
  '{"text":"hello opencode","files":[],"agents":[],"time":{"created":1763000001000}}');
insert into session_message values ('msg_p2', 'ses_plain', 'assistant', 2, 1763000001500, 1763000002000,
  '{"model":{"id":"fledge-alpha-free","providerID":"opencode"},"agent":"build","content":[{"type":"reasoning","text":"think"},{"type":"text","text":"hi"}],"rawFinish":"stop","finish":"stop","tokens":{"input":193,"output":154,"reasoning":0,"cache":{"read":7040,"write":0}},"cost":0,"time":{"created":1763000001500,"streamed":1763000001800,"completed":1763000002000}}');
insert into session_message values ('msg_p3', 'ses_plain', 'idle', 3, 1763000002100, 1763000002100, '{}');

insert into session_v2 values ('ses_fork', null, 'ses_plain', '/tmp/demo', '2.0.26', 1763000004000);
insert into session_message values ('msg_f1', 'ses_fork', 'user', 1, 1763000001000, 1763000001000,
  '{"text":"hello opencode","time":{"created":1763000001000}}');
insert into session_message values ('msg_f2', 'ses_fork', 'assistant', 2, 1763000001500, 1763000002000,
  '{"model":{"id":"fledge-alpha-free","providerID":"opencode"},"content":[{"type":"reasoning","text":"think"},{"type":"text","text":"hi"}],"rawFinish":"stop","tokens":{"input":193,"output":154,"reasoning":0,"cache":{"read":7040,"write":0}},"time":{"created":1763000001500,"completed":1763000002000}}');
insert into session_message values ('msg_f3', 'ses_fork', 'user', 3, 1763000005000, 1763000005000,
  '{"text":"forked","time":{"created":1763000005000}}');
insert into session_message values ('msg_f4', 'ses_fork', 'assistant', 4, 1763000005500, 1763000006000,
  '{"model":{"id":"fledge-alpha-free","providerID":"opencode"},"content":[{"type":"text","text":"fork reply"}],"rawFinish":"end_turn","tokens":{"input":10,"output":5,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"created":1763000005500,"completed":1763000006000}}');

insert into session_v2 values ('ses_tools', null, null, '/tmp/tools', '2.0.26', 1763000006900);
insert into session_message values ('msg_t1', 'ses_tools', 'user', 1, 1763000007000, 1763000007000,
  '{"text":"read a.txt","time":{"created":1763000007000}}');
insert into session_message values ('msg_t2', 'ses_tools', 'assistant', 2, 1763000007100, 1763000007200,
  '{"model":{"id":"gpt-5","providerID":"opencode"},"content":[{"type":"tool","id":"c1","name":"read","state":{"status":"completed","input":{"path":"a.txt"},"content":[{"type":"text","text":"file body"}]}},{"type":"tool","id":"c2","name":"shell","state":{"status":"error","input":{"cmd":"false"},"error":{"message":"exit 1"}}},{"type":"tool","id":"c3","name":"subagent","state":{"status":"completed","input":{"prompt":"go"},"content":[{"type":"text","text":"<subagent sessionID=\"ses_child\" state=\"completed\">"},{"type":"text","text":"done"}]}}],"finish":"tool-calls","rawFinish":"tool_calls","tokens":{"input":4384,"output":8,"reasoning":14,"cache":{"read":0,"write":0}},"time":{"created":1763000007100,"completed":1763000007200}}');
insert into session_message values ('msg_t3', 'ses_tools', 'assistant', 3, 1763000007300, 1763000007400,
  '{"model":{"id":"gpt-5","providerID":"opencode"},"content":[{"type":"text","text":"done"}],"finish":"stop","rawFinish":"stop","tokens":{"input":50,"output":3,"reasoning":0,"cache":{"read":10,"write":0}},"time":{"created":1763000007300,"completed":1763000007400}}');
insert into session_message values ('msg_t4', 'ses_tools', 'model-switched', 4, 1763000007500, 1763000007500, '{"model":{"id":"gpt-5"}}');
insert into session_message values ('msg_t5', 'ses_tools', 'user', 5, 1763000007550, 1763000007550, 'not json');
insert into session_message values ('msg_t6', 'ses_tools', 'bogus', 6, 1763000007560, 1763000007560, '{}');
insert into session_message values ('msg_t7', 'ses_tools', 'assistant', 7, 1763000007600, 1763000007700,
  '{"model":{"id":"gpt-5","providerID":"opencode"},"content":[],"finish":"error","error":{"type":"api","message":"overloaded","status":529},"time":{"created":1763000007600}}');

insert into session_v2 values ('ses_child', 'ses_tools', null, '/tmp/tools', '2.0.26', 1763000007900);
insert into session_message values ('msg_c1', 'ses_child', 'user', 1, 1763000008000, 1763000008000,
  '{"text":"go","time":{"created":1763000008000}}');
insert into session_message values ('msg_c2', 'ses_child', 'assistant', 2, 1763000008500, 1763000009000,
  '{"model":{"id":"gpt-5","providerID":"opencode"},"content":[{"type":"text","text":"child reply"}],"rawFinish":"stop","tokens":{"input":20,"output":2,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"created":1763000008500,"completed":1763000009000}}');

insert into session_v2 values ('ses_orphan', 'ses_gone', null, '/tmp/demo', '2.0.26', 1763000000050);
insert into session_message values ('msg_o1', 'ses_orphan', 'user', 1, 1763000000100, 1763000000100,
  '{"text":"orphan","time":{"created":1763000000100}}');
insert into session_message values ('msg_o2', 'ses_orphan', 'assistant', 2, 1763000000150, 1763000000200,
  '{"model":{"id":"gpt-5","providerID":"openai"},"content":[{"type":"text","text":"orphan reply"}],"rawFinish":"stop","time":{"created":1763000000150,"completed":1763000000200}}');

insert into session_v2 values ('ses_empty', null, null, '/tmp/demo', '2.0.26', 1763000000000);
