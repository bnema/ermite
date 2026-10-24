import assert from 'node:assert/strict';
import { test } from 'node:test';
globalThis.window = globalThis;
globalThis.document = {readyState:'complete',querySelectorAll:()=>[],addEventListener(){}};
await import('../static/js/md-editor.js');
const C = window.ermiteMdEditor.core;
const changed = (s,a,b,k) => { const c=C.format(s,a,b,k); return s.slice(0,c.start)+c.text+s.slice(c.end); };
test('inline formats wrap and unwrap selected text and adjacent markers',()=>{
  for (const k of ['bold','italic','code','fence','link']) {
    const wrapped=changed('hello',0,5,k);
    assert.equal(changed(wrapped,0,wrapped.length,k),'hello');
  }
  assert.equal(changed('**hello**',2,7,'bold'),'hello');
  assert.deepEqual(C.format('',0,0,'bold').select,[2,2]);
});
test('block formats toggle whole selected lines, not the next line',()=>{
  for (const k of ['heading','quote','bullet','number','task']) {
    const wrapped=changed('one\ntwo',0,7,k);
    assert.equal(changed(wrapped,0,wrapped.length,k),'one\ntwo');
  }
  assert.equal(changed('one\ntwo',0,4,'bullet'),'- one\ntwo');
});
test('list continuation increments numbers and resets checked tasks',()=>{
  for (const [s,want] of [['- abc','\n- '],['  9. abc','\n  10. '],['- [x] abc','\n- [ ] '],['* abc','\n* ']]) {
    assert.equal(C.continuation(s,s.length).text,want);
  }
  for (const s of ['- ','1. ','- [ ] ']) assert.deepEqual(C.continuation(s,s.length),{start:0,end:s.length,text:''});
  assert.equal(C.continuation('plain',5),null);
  assert.equal(C.continuation('- abc',2,4),null);
});
test('highlighting preserves escaped user text, including malicious markup',()=>{
  const html=C.highlight('## Title\n- item\n**bold** *italic* `code`\n> quote\n[x](javascript:evil)\n```\n<script>&\n```');
  for (const cls of ['heading','marker','bold','italic','code','quote','link']) assert.ok(html.includes('md-'+cls));
  assert.ok(html.includes('&lt;script&gt;&amp;'));
  assert.ok(!html.includes('<script>'));
  assert.ok(!html.includes('href='));
  assert.ok(C.highlight('<img onerror="x">').includes('&quot;'));
});
test('highlighting keeps all markers and line breaks',()=>{
  const text='## hi\n**yes**\n```\n*a*\n```\n';
  assert.equal(C.highlight(text).replace(/<[^>]+>/g,''),text);
  assert.ok(!C.highlight('```\n**raw**\n```').includes('md-bold'));
});

test('block format at zero before a leading newline never inverts its range',()=>{
  const c=C.format('\ntext',0,0,'bullet');
  assert.deepEqual(c,{start:0,end:0,text:'- ',select:[0,2]});
  assert.equal(changed('\ntext',0,0,'bullet'),'- \ntext');
});
test('block toggles recognize numbered and checked task prefixes',()=>{
  assert.equal(changed('1. one\n2. two',0,13,'number'),'one\ntwo');
  assert.equal(changed('one\ntwo\nthree',0,13,'number'),'1. one\n2. two\n3. three');
  const tasks='- [ ] one\n- [x] two\n- [X] three';
  assert.equal(changed(tasks,0,tasks.length,'task'),'one\ntwo\nthree');
});
test('ending a list preserves the empty line for subsequent text',()=>{
  for (const marker of ['- ','1. ','- [ ] ','- [x] ']) {
    const value='- item one\n- item two\n'+marker;
    const c=C.continuation(value,value.length);
    assert.deepEqual(c,{start:value.lastIndexOf('\n')+1,end:value.length,text:''});
    const result=value.slice(0,c.start)+c.text+value.slice(c.end);
    assert.equal(result+'> quote','- item one\n- item two\n> quote');
  }
  assert.equal(C.continuation('\n',0),null);
  assert.equal(C.continuation('\n',1),null);
});

test('editor layers share desktop and mobile typography rules',async()=>{
  // Static cascade regression: both layers must be in the same selector list,
  // including the higher-specificity MR mobile override loaded after 61-*.
  const {readFile} = await import('node:fs/promises');
  const desktop=await readFile(new URL('../static/css/61-md-editor.css',import.meta.url),'utf8');
  const mobile=await readFile(new URL('../static/css/85-mobile.css',import.meta.url),'utf8');
  assert.match(desktop,/\.md-surface \.md-backdrop, \.md-surface textarea\s*\{[^}]*font: var\(--fs\)\/1\.6 var\(--mono\)/);
  assert.match(mobile,/@media \(max-width: 720px\)/);
  assert.match(mobile,/body:has\(#diffs\[data-mr\]\) :is\(input, textarea, select, \.md-backdrop\)\s*\{ font-size: 16px; line-height: 1\.6; \}/);
});
