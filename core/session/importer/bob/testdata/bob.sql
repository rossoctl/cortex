-- A scrubbed bob.db: only the columns that the Bob source reads, plus title.
-- Times are milliseconds since the epoch. As in a real bob.db, a subagent's task row has no
-- messages: its turns are in the parent's spawn_subagent tool row (data.messages), and a
-- start_subtask tool row holds a copy of the subtask's conversation. Tree times: t-tools 1763000009000 (from its
-- child), t-orphan 1763000004000, t-plain 1763000002000.
create table tasks (
  id text primary key,
  parent_id text,
  title text not null default '',
  directory text not null,
  version text,
  env text,
  task_type text not null default 'normal'
);
create table messages (
  id text primary key,
  task_id text not null,
  role text not null,
  data text not null,
  created_at integer not null
);

insert into tasks values ('t-plain', null, 'hello bob', '', '2.1.0', '{"workspace":"/tmp/demo","model":{"id":"premium-ide"}}', 'normal');
insert into tasks values ('t-tools', null, 'read a file', '/tmp/tools', null, '{"model":{"id":"premium-ide"}}', 'normal');
insert into tasks values ('t-child', 't-tools', 'child', '', null, '{"model":{"id":"premium-ide"}}', 'subtask');
insert into tasks values ('t-sub', 't-tools', 'subagent', '', null, '{}', 'subagent');
insert into tasks values ('t-orphan', 't-gone', 'orphan', '', null, '{"model":{"id":"premium-ide"}}', 'subtask');
insert into tasks values ('t-empty', null, 'empty', '', null, '{}', 'normal');

insert into messages values ('m-plain-1', 't-plain', 'system', '{"content":"You are Bob."}', 1763000001000);
insert into messages values ('m-plain-2', 't-plain', 'user', '{"content":"hello bob"}', 1763000001000);
insert into messages values ('m-plain-3', 't-plain', 'assistant', '{"content":"hi","_meta":{"spend":{"cost":0.000018,"contextTokens":9}}}', 1763000002000);

insert into messages values ('m-orphan-1', 't-orphan', 'user', '{"content":"do the subtask"}', 1763000003000);
insert into messages values ('m-orphan-2', 't-orphan', 'assistant', '{"content":"done"}', 1763000004000);

insert into messages values ('m-tools-1', 't-tools', 'user', '{"content":"read a.go","availableTools":["read_file","spawn_subagent"]}', 1763000005000);
insert into messages values ('m-tools-2', 't-tools', 'assistant', '{"content":"","toolCalls":[{"id":"c1","name":"read_file","arguments":{"path":"a.go"}},{"id":"c2","name":"spawn_subagent","arguments":{"message":"list the files"}}],"thinkingBlocks":[{"type":"thinking","thinking":"look first","signature":"sig"}],"_meta":{"spend":{"cost":0.0002,"contextTokens":80}}}', 1763000006000);
insert into messages values ('m-tools-3', 't-tools', 'tool', '{"content":"package a"}', 1763000007000);
insert into messages values ('m-tools-4', 't-tools', 'tool', '{"content":"a.go","_meta":{"subagentId":"t-sub","spend":{"cost":0.0001,"contextTokens":50}},"messages":[{"id":"n1","role":"system","content":"You are a subagent."},{"id":"n2","role":"user","content":"list the files","availableTools":["list_files","spawn_subagent"]},{"id":"n3","role":"assistant","content":"","toolCalls":[{"id":"d1","name":"spawn_subagent","arguments":{"message":"look"}}],"_meta":{"timestamp":1763000006200,"spend":{"cost":0.00003,"contextTokens":30}}},{"id":"n4","role":"tool","content":"looked","_meta":{"subagentId":"s-inner","spend":{"cost":0.00001,"contextTokens":10},"timestamp":1763000006400},"messages":[{"id":"i1","role":"user","content":"look"},{"id":"i2","role":"assistant","content":"looked","_meta":{"timestamp":1763000006350,"spend":{"cost":0.00001,"contextTokens":10}}}]},{"id":"n5","role":"assistant","content":"a.go","_meta":{"timestamp":1763000006500,"spend":{"cost":0.00007,"contextTokens":50}}}]}', 1763000007000);
insert into messages values ('m-tools-5', 't-tools', 'tool', '{"content":"tool-extra","messages":[{"id":"k1","role":"user","content":"list the files"},{"id":"k2","role":"assistant","content":"","toolCalls":[{"id":"e1","name":"spawn_subagent","arguments":{"message":"count"}}],"_meta":{"timestamp":1763000006600,"spend":{"cost":0.00002,"contextTokens":20}}},{"id":"k3","role":"tool","content":"3","_meta":{"subagentId":"s-copy","spend":{"cost":0.00005,"contextTokens":40},"timestamp":1763000006800},"messages":[{"id":"j1","role":"user","content":"count"},{"id":"j2","role":"assistant","content":"3","_meta":{"timestamp":1763000006700,"spend":{"cost":0.00005,"contextTokens":40}}}]}]}', 1763000007000);
insert into messages values ('m-tools-6', 't-tools', 'user', 'not json', 1763000007500);
insert into messages values ('m-tools-7', 't-tools', 'assistant', '{"content":"a.go holds package a","stop":true,"_meta":{"stopReason":"stop","spend":{"cost":0,"contextTokens":120}}}', 1763000008000);

insert into messages values ('m-child-1', 't-child', 'user', '{"content":"list the files"}', 1763000006500);
insert into messages values ('m-child-2', 't-child', 'assistant', '{"content":"","toolCalls":[{"id":"e1","name":"spawn_subagent","arguments":{"message":"count"}}],"_meta":{"spend":{"cost":0.00002,"contextTokens":20}}}', 1763000006600);
insert into messages values ('m-child-3', 't-child', 'tool', '{"content":"3","_meta":{"subagentId":"s-copy","spend":{"cost":0.00005,"contextTokens":40}},"messages":[{"id":"j1","role":"user","content":"count"},{"id":"j2","role":"assistant","content":"3","_meta":{"timestamp":1763000006700,"spend":{"cost":0.00005,"contextTokens":40}}}]}', 1763000006800);
insert into messages values ('m-child-4', 't-child', 'assistant', '{"content":"a.go","_meta":{"spend":{"cost":0.0001,"contextTokens":50}}}', 1763000009000);
