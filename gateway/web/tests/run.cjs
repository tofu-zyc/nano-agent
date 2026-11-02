const { mkdtempSync, rmSync, writeFileSync } = require('node:fs');
const { tmpdir } = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { test, after } = require('node:test');
const assert = require('node:assert/strict');

const compiled = mkdtempSync(path.join(tmpdir(), 'nano-web-test-'));
after(() => rmSync(compiled, { recursive: true, force: true }));
const result = spawnSync(process.execPath, [require.resolve('typescript/bin/tsc'), '--ignoreConfig', '--target', 'ES2022', '--module', 'CommonJS', '--skipLibCheck', '--outDir', compiled, 'src/lib/conv.ts', 'src/lib/jobs.ts', 'src/lib/stage.ts'], { encoding: 'utf8' });
if (result.status !== 0) throw new Error(result.stdout + result.stderr);
writeFileSync(path.join(compiled, 'package.json'), '{"type":"commonjs"}');
const { buildBlocks } = require(path.join(compiled, 'conv.js'));
const stage = require(path.join(compiled, 'stage.js'));
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

test('stage cues follow the same timing and splitting as the exported subtitles', () => {
  const long = '第一句话说得比较长比较长比较长比较长。第二句也不短不短不短不短不短？第三句同样很长很长很长很长很长！';
  const items = [
    { kind: 'say', who: '小鲍', text: '好', line: 1 },
    { kind: 'narrate', text: '长'.repeat(39), line: 2 },
    { kind: 'say', who: '小猫', text: '字'.repeat(41), line: 3 },
    { kind: 'say', who: '小纳', text: long, line: 4 },
    { kind: 'narrate', text: 'x\n y  z', line: 5 },
  ];
  const cues = stage.buildCues(items, { inner: false, channels: false });
  assert.deepEqual(cues.slice(0, 3).map((c) => [c.start, c.end]), [[0, 1500], [1800, 9600], [9900, 17900]]);
  assert.equal(cues.length, 7); // 三条普通 + 长句拆三条 + 最后一条
  assert.ok(cues[3].text.startsWith('小纳：第一句话') && !cues[4].text.includes('小纳') && !cues[5].text.includes('小纳'));
  assert.equal(cues[6].text, 'x y z');
});

test('stage cues obey the inner and channel switches; cues and system lines never show', () => {
  const items = [
    { kind: 'inner', who: '小鲍', text: '他在撒谎', line: 2 },
    { kind: 'say', who: '小鲍', text: '我同意', line: 2 },
    { kind: 'say', who: '小猫', channel: '密谈', text: '只告诉你', line: 3 },
    { kind: 'inner', who: '小猫', channel: '密谈', text: '他不信', line: 3 },
    { kind: 'cue', to: '小鲍', text: '走到窗边', line: 5 },
    { kind: 'system', text: '先暂停', line: 6 },
  ];
  const texts = (o) => stage.buildCues(items, o).map((c) => c.text);
  assert.deepEqual(texts({ inner: false, channels: false }), ['小鲍：我同意']);
  assert.deepEqual(texts({ inner: true, channels: false }), ['（小鲍心里）他在撒谎', '小鲍：我同意']);
  assert.deepEqual(texts({ inner: false, channels: true }), ['小鲍：我同意', '小猫（私下）：只告诉你']);
  assert.deepEqual(texts({ inner: true, channels: true }), ['（小鲍心里）他在撒谎', '小鲍：我同意', '小猫（私下）：只告诉你', '（小猫心里）他不信']);
});

test('stage items put the thought before the line, keep silent thoughts in place, and find forkable lines', () => {
  const recs = [
    { kind: 'line', line: 1, say: 'narrate', name: '旁白', text: '夜深了' },
    { kind: 'cursors', line: 1, cursors: {} },
    { kind: 'line', line: 2, say: 'say', from: 'bob', name: '小鲍', text: '我同意' },
    { kind: 'run', member: 'bob', said: 2, inner: '他在撒谎' },
    { kind: 'run', member: 'cat', said: 0, inner: '先观察' },
    { kind: 'line', line: 3, say: 'system', text: '[系统] 先暂停' },
  ];
  const items = stage.buildItems(recs, (c) => c, (id) => ({ bob: '小鲍', cat: '小猫' })[id] ?? id);
  assert.deepEqual(items.map((i) => i.kind + ':' + i.text), ['narrate:夜深了', 'inner:他在撒谎', 'say:我同意', 'inner:先观察', 'system:先暂停']);
  assert.deepEqual([...stage.forkable(recs)], [1]);
});
