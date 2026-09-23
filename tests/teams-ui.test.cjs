const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');

function workspace(role = 'owner') {
  const elements = new Map(), listeners = new Map();
  const element = selector => {
    if (!elements.has(selector)) elements.set(selector, {
      innerHTML: '', textContent: '', hidden: false,
      addEventListener() {}, showModal() {},
    });
    return elements.get(selector);
  };
  const context = vm.createContext({
    document: { querySelector: element, addEventListener: (name, fn) => {
      if (!listeners.has(name)) listeners.set(name, []);
      listeners.get(name).push(fn);
    } },
    localStorage: { getItem: () => '', setItem() {} },
  });
  const source = fs.readFileSync(path.join(__dirname, '../web/app.js'), 'utf8').replace(/\ninit\(\);\s*$/, '\n');
  vm.runInContext(source, context);
  const data = {
    projects: [], users: [{ id: 'me', name: 'Test User' }],
    organizations: [{ id: 'org', name: 'New workspace', members: { me: role } }],
    teams: [], notifications: [], issues: [], merge_requests: [], pipelines: [], activity: [], audit: [],
  };
  vm.runInContext(`data=${JSON.stringify(data)};me='me';currentOrg='org';currentProject='';view='members';`, context);
  return { run: code => vm.runInContext(code, context), element, listeners };
}

test('Members & teams navigation works before creating a project', async () => {
  const { run, element, listeners } = workspace();
  run("view='dashboard'");
  const target = { closest: selector => selector === '[data-view]' ? { dataset: { view: 'members' } } : null };
  for (const listener of listeners.get('click')) await listener({ target });
  assert.match(element('#content').innerHTML, /People &amp; teams/);
  assert.match(element('#content').innerHTML, /No teams yet/);
  assert.match(element('#content').innerHTML, /data-action="team"/);
  assert.doesNotMatch(element('#content').innerHTML, /Your first project starts here/);
});

test('projectless teams render and create/edit forms show workspace members', () => {
  const { run, element } = workspace();
  run(`data.teams=[{id:'team',org_id:'org',name:'Platform',members:['me']}];render();teamModal('team');`);
  assert.match(element('#content').innerHTML, /Platform/);
  assert.match(element('#modal-body').innerHTML, /name="members" multiple/);
  assert.match(element('#modal-body').innerHTML, /value="me" selected/);
  assert.equal(element('#modal-title').textContent, 'Edit team');
});

test('workspace guests can read teams without seeing management actions', () => {
  const { run, element } = workspace('guest');
  run('render()');
  assert.match(element('#content').innerHTML, /People &amp; teams/);
  assert.doesNotMatch(element('#content').innerHTML, /data-action="team"|data-action="member"/);
});

test('manage roles without a project only offers organization scope', async () => {
  const { element, listeners } = workspace();
  const target = { closest: selector => selector === '[data-action]' ? { dataset: { action: 'member' } } : null };
  for (const listener of listeners.get('click')) await listener({ target });
  assert.match(element('#modal-body').innerHTML, /value="organization.member"/);
  assert.doesNotMatch(element('#modal-body').innerHTML, /value="project.member"/);
});
