const { mkdtempSync, rmSync, writeFileSync } = require('node:fs');
const { tmpdir } = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { test, after } = require('node:test');
const assert = require('node:assert/strict');

const compiled = mkdtempSync(path.join(tmpdir(), 'nano-web-test-'));
after(() => rmSync(compiled, { recursive: true, force: true }));
const result = spawnSync(process.execPath, [require.resolve('typescript/bin/tsc'), '--ignoreConfig', '--target', 'ES2022', '--module', 'CommonJS', '--skipLibCheck', '--outDir', compiled, 'src/lib/conv.ts', 'src/lib/jobs.ts'], { encoding: 'utf8' });
if (result.status !== 0) throw new Error(result.stdout + result.stderr);
writeFileSync(path.join(compiled, 'package.json'), '{"type":"commonjs"}');
const { buildBlocks } = require(path.join(compiled, 'conv.js'));
const call = (id, name) => ({id,type:'function',function:{name,arguments:'{}'}});
const assistant = (run, turn, calls) => ({role:'assistant',run_id:run,turn_id:turn,tool_calls:calls});
const tool = (run, turn, id, text) => ({role:'tool',run_id:run,turn_id:turn,tool_call_id:id,content:text});

test('out of order completion, repeated call IDs, and a stale duplicate stay with their full identity', () => {
  const blocks = buildBlocks([
    assistant('r1',1,[call('same','Read'),call('second','Shell')]),
    tool('r1',1,'second','[执行失败] shell failed'),
    tool('r1',1,'same','first result'),
    assistant('r1',2,[call('same','Write')]),
    tool('r1',2,'same','second result'),
    assistant('r2',1,[call('same','Grep')]),
    tool('r2',1,'same','third result'),
    tool('r1',1,'same','stale duplicate'),
  ]);
  const replies = blocks.filter(b => b.type === 'assistant');
  assert.equal(replies[0].tools[0].result,'first result');
  assert.equal(replies[0].tools[1].result,'shell failed');
  assert.equal(replies[0].tools[1].ok,false);
  assert.equal(replies[1].tools[0].result,'second result');
  assert.equal(replies[2].tools[0].result,'third result');
});

test('legacy history without run identity remains readable', () => {
  const result = buildBlocks([{role:'assistant',tool_calls:[call('old','Read')]},{role:'tool',tool_call_id:'old',content:'old result'}]);
  assert.equal(result[0].tools[0].result,'old result');
});

test('unaccepted proposals stay inspectable without becoming executed tool cards', () => {
  const proposal = call('unaccepted','Write');
  const result = buildBlocks([{role:'assistant',proposed_calls:[proposal],kind:'unaccepted_tool_proposal'}]);
  assert.equal(result[0].tools.length,0);
  assert.deepEqual(result[0].proposed,[proposal]);
});


const { JobLog, mergeJob } = require(path.join(compiled, 'jobs.js'));
const job = (id, revision, status = 'running', node = 'node') => ({ job_id: id, node, revision, status });

test('a late job response cannot roll back completion or confuse node-local IDs', () => {
  const done = job('same', 5, 'succeeded');
  assert.equal(mergeJob(done, job('same', 4, 'running')), done);
  assert.equal(mergeJob(done, job('same', 6, 'running')), done);
  assert.equal(mergeJob(done, job('same', 6, 'succeeded')).revision, 6);
  assert.equal(mergeJob(done, job('same', 1, 'running', 'other')).node, 'other');
});

const page = (cursor, bytes, end, stream='stdout') => ({
  chunks: [{cursor, next_cursor:end, stream, data:Buffer.from(bytes).toString('base64')}],
  next_cursor:end, output_cursor:end, truncated_before:0, gap:false,
});
test('job output preserves a UTF-8 character across chunks and separate cursor reads', () => {
  const log = new JobLog();
  const bytes = Buffer.from('中文🙂');
  log.append(page(0, bytes.subarray(0,2), 2));
  assert.equal(log.text, '');
  log.append(page(2, bytes.subarray(2,7), 7));
  assert.equal(log.text, '中文');
  log.append(page(7, bytes.subarray(7), 10), true);
  assert.equal(log.text, '中文🙂');
  assert.equal(log.cursor, 10);
});

test('job output exposes rotation gaps, rejects backwards pages and bounds its live text', () => {
  const log = new JobLog();
  const block = Buffer.alloc(65536, 65);
  log.append(page(0, block, 65536));
  log.append(page(65536, block, 131072));
  log.append(page(131072, block, 196608));
  assert.equal(log.text.length, 131072);
  assert.throws(() => log.append(page(0, Buffer.from('old'), 3)));
  const rotated = page(200000, Buffer.from('new'), 200003);
  rotated.truncated_before=200000;
  rotated.gap=true;
  log.append(rotated);
  assert.equal(log.gap,true);
  assert.equal(log.text.endsWith('new'),true);
  assert.throws(() => log.append(page(200003, Buffer.alloc(65537), 265540)));
});

test('宿主报告卡片：结构化字段画卡片，普通用户同样正文仍是气泡', () => {
  const report = { kind: 'branch_report', branch_sid: 'br-1', parent_sid: 'main', title: '日志格式',
    conclusion: '按天轮转，保留 7 天。', checkpoint: 3, status: 'completed', evidence: ['跑过 ls'],
    snapshot_id: 'snap-1', at: 100 };
  const blocks = buildBlocks([
    { role: 'user', content: '[分支报告 br-1「日志格式」]\n结论：按天轮转', kind: 'branch_report', report },
    { role: 'user', content: '[分支报告 br-1「日志格式」]\n结论：按天轮转' },
  ]);
  assert.equal(blocks[0].type, 'report');
  assert.equal(blocks[0].report.branch_sid, 'br-1');
  assert.equal(blocks[0].report.checkpoint, 3);
  // 用户消息只有正文、没有结构化字段：还是普通气泡，不冒充报告
  assert.equal(blocks[1].type, 'user');
  assert.match(blocks[1].text, /分支报告/);
  // 两份报告（父会话忙时连续引入）各画各的卡片，内容都在
  const two = buildBlocks([
    { role: 'user', content: 'x', kind: 'branch_report', report: { ...report, branch_sid: 'br-1', conclusion: '结论一' } },
    { role: 'user', content: 'y', kind: 'branch_report', report: { ...report, branch_sid: 'br-2', conclusion: '结论二' } },
  ]);
  const cards = two.filter(b => b.type === 'report');
  assert.equal(cards.length, 2);
  assert.equal(cards[0].report.conclusion, '结论一');
  assert.equal(cards[1].report.conclusion, '结论二');
});
