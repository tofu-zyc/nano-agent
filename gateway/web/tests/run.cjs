const { mkdtempSync, rmSync, writeFileSync } = require('node:fs');
const { tmpdir } = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { test, after } = require('node:test');
const assert = require('node:assert/strict');

const compiled = mkdtempSync(path.join(tmpdir(), 'nano-web-test-'));
after(() => rmSync(compiled, { recursive: true, force: true }));
const result = spawnSync(process.execPath, [require.resolve('typescript/bin/tsc'), '--ignoreConfig', '--target', 'ES2022', '--module', 'CommonJS', '--skipLibCheck', '--outDir', compiled, 'src/lib/conv.ts'], { encoding: 'utf8' });
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
