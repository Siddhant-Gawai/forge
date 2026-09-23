const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');

function page() {
  const context = vm.createContext({
    document: { querySelector: () => ({ addEventListener() {} }), addEventListener() {} },
    localStorage: { getItem: () => '' },
  });
  for (const file of ['app.js', 'reviews.js']) {
    let source = fs.readFileSync(path.join(__dirname, '../web', file), 'utf8');
    if (file === 'app.js') source = source.replace(/\ninit\(\);\s*$/, '\n');
    vm.runInContext(source, context);
  }
  vm.runInContext(`
    data={projects:[{id:'p',org_id:'o',name:'Repo',members:{},required_approvals:1,require_pipeline:true,repository:{provider:'github',full_name:'acme/repo'}}],organizations:[{id:'o',members:{sam:'maintainer'}}],users:[{id:'sam',name:'Sam'}]};
    me='sam';currentProject='p';currentOrg='o';
    var requestFixture={id:'m',project_id:'p',title:'Review',source:'feature',target:'main',state:'open',comments:[],remote:{number:7,head_sha:'aaa',base_sha:'bbb',author_id:11,author:'author',url:'https://github.com/acme/repo/pull/7',mergeable:true,merge_status:'clean',ci:'passed',reviews:[{user_id:'sam',provider_user_id:22,head_sha:'aaa',decision:'approve',body:'',created_at:''}]}};
  `, context);
  return expression => vm.runInContext(expression, context);
}

test('review display invalidates approvals after new commits', () => {
  const run = page();
  assert.equal(run('reviewBlocks(requestFixture).blocks.length'), 0);
  run("requestFixture.remote.head_sha='ccc'");
  assert.equal(run('reviewBlocks(requestFixture).approvals'), 0);
  assert.match(run('remoteDetail(requestFixture)'), /Older commit/);
});

test('diff escapes executable content and anchors old/new lines', () => {
  const run = page();
  const html = run(`renderDiff(requestFixture,{path:'<script>.go',status:'modified',patch:'@@ -1 +1 @@\\n-old\\n+<img src=x onerror=alert(1)>'},0)`);
  assert.ok(!html.includes('<img'));
  assert.match(html, /&lt;img/);
  assert.match(html, /data-side="old"/);
  assert.match(html, /data-side="new"/);
  assert.match(html, /data-line="1"/);
});

test('unresolved old comments and change requests remain visible blockers', () => {
  const run = page();
  run("requestFixture.remote.reviews[0].decision='request_changes';requestFixture.remote.reviews[0].head_sha='old';requestFixture.comments=[{resolved:false,commit_sha:'old'}]");
  const blocks = run('reviewBlocks(requestFixture).blocks.join(" ")');
  assert.match(blocks, /requested changes/);
  assert.match(blocks, /Resolve all review discussions/);
});
