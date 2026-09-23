const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {JSDOM} = require('jsdom');
const source = fs.readFileSync(path.join(__dirname, '../../web/selects.js'), 'utf8');

async function setup(t, html) {
  const dom = new JSDOM(html, {runScripts: 'outside-only', pretendToBeVisual: true});
  const {window: w} = dom;
  const observers = [];
  const Observer = w.MutationObserver;
  w.MutationObserver = class extends Observer {
    constructor(callback) { super(callback); observers.push(this); }
  };
  t.after(async () => {
    observers.forEach(observer => observer.disconnect());
    await new Promise(resolve => setTimeout(resolve, 0));
    dom.window.close();
  });
  // jsdom has no layout/top-layer implementation. These shims only emulate visibility.
  w.HTMLElement.prototype.showPopover = function() { this.dataset.open = 'true'; };
  w.HTMLElement.prototype.hidePopover = function() { delete this.dataset.open; };
  w.HTMLElement.prototype.scrollIntoView = function() {};
  const matches = w.Element.prototype.matches;
  w.Element.prototype.matches = function(selector) { return selector === ':popover-open' ? this.dataset.open === 'true' : matches.call(this, selector); };
  w.eval(source);
  const tick = () => new Promise(resolve => setTimeout(resolve, 0));
  await tick();
  const doc = w.document;
  const open = () => { doc.querySelector('.select-trigger').click(); return doc.querySelector('.select-panel'); };
  const key = value => doc.activeElement.dispatchEvent(new w.KeyboardEvent('keydown', {key: value, bubbles: true, cancelable: true}));
  return {w, doc, tick, open, key};
}

test('single selection preserves native form data and change handlers', async t => {
  const {w, doc, open} = await setup(t, '<form><label>Role<select name="role"><option>guest</option><option>developer</option><option>maintainer</option></select></label></form>');
  let changes = 0;
  doc.querySelector('select').addEventListener('change', () => changes++);
  const panel = open();
  assert.equal(panel.querySelectorAll('[role=option]').length, 3);
  panel.querySelectorAll('[role=option]')[1].click();
  assert.equal(new w.FormData(doc.querySelector('form')).get('role'), 'developer');
  assert.equal(changes, 1);
  assert.equal(doc.querySelector('.select-value').textContent, 'developer');
  assert.equal(doc.querySelector('.select-panel'), null);
  assert.equal(doc.activeElement, doc.querySelector('.select-trigger'));
});

test('search, keyboard selection, and escaping use real DOM nodes', async t => {
  const {w, doc, open, key} = await setup(t, '<label>Team<select><option>Platform</option><option>Backend</option><option>&lt;img src=x onerror=alert(1)&gt;</option></select></label>');
  const panel = open();
  assert.equal(panel.querySelector('img'), null);
  const search = panel.querySelector('input');
  search.value = 'back'; search.dispatchEvent(new w.Event('input', {bubbles: true}));
  assert.equal(panel.querySelectorAll('[role=option]').length, 1);
  key('Enter');
  assert.equal(doc.querySelector('select').value, 'Backend');
  open(); key('Escape');
  assert.equal(doc.querySelector('.select-panel'), null);
});

test('multiple selection toggles without modifier keys and submits all values', async t => {
  const {w, doc, open} = await setup(t, '<form><label>Members<select name="members" multiple><option value="a" selected>Alex</option><option value="s">Sam</option><option value="j">Jordan</option></select></label></form>');
  const panel = open();
  panel.querySelectorAll('[role=option]')[1].click();
  assert.deepEqual([...new w.FormData(doc.querySelector('form')).getAll('members')], ['a', 's']);
  assert.equal(panel.querySelectorAll('[aria-selected=true]').length, 2);
  assert.match(doc.querySelector('.select-value').textContent, /Alex, Sam/);
  panel.querySelectorAll('[role=option]')[0].click();
  assert.deepEqual([...new w.FormData(doc.querySelector('form')).getAll('members')], ['s']);
  panel.querySelector('.select-clear').click();
  assert.equal(doc.querySelector('select').selectedOptions.length, 0);
  panel.querySelector('.select-done').click();
  assert.equal(doc.querySelector('.select-panel'), null);
});

test('dynamic forms and replaced options refresh without duplicate controls', async t => {
  const {doc, tick, open} = await setup(t, '<div id="content"><label>Project<select><option>One</option></select></label></div>');
  const select = doc.querySelector('select');
  select.innerHTML = '<option selected>Two</option><option>Three</option>';
  await tick();
  assert.equal(doc.querySelector('.select-value').textContent, 'Two');
  assert.equal(doc.querySelectorAll('.select-trigger').length, 1);
  open();
  doc.querySelector('#content').innerHTML = '<label>New form<select><option>Fresh</option></select></label>';
  await tick();
  assert.equal(doc.querySelector('.select-panel'), null);
  assert.equal(doc.querySelectorAll('.select-trigger').length, 1);
  assert.equal(doc.querySelector('.select-value').textContent, 'Fresh');
});

test('menus work inside dialogs and close on dialog dismissal or outside clicks', async t => {
  const {w, doc, open} = await setup(t, '<dialog open><form><label>State<select><option>open</option><option>closed</option></select></label></form></dialog>');
  assert.equal(open().parentElement, doc.querySelector('dialog'));
  doc.querySelector('dialog').dispatchEvent(new w.Event('close'));
  assert.equal(doc.querySelector('.select-panel'), null);
  open();
  doc.body.dispatchEvent(new w.Event('pointerdown', {bubbles: true}));
  assert.equal(doc.querySelector('.select-panel'), null);
});

test('disabled controls, required validation and form reset retain native behavior', async t => {
  const {w, doc, tick, open} = await setup(t, '<form><label>Project<select name="project" required><option value="">Choose</option><option value="p">Project</option><option disabled value="x">Unavailable</option></select></label></form>');
  assert.equal(doc.querySelector('form').checkValidity(), false);
  assert.equal(doc.querySelector('.select-trigger').getAttribute('aria-invalid'), 'true');
  const panel = open();
  panel.querySelector('[aria-disabled=true]').click();
  assert.equal(doc.querySelector('select').value, '');
  panel.querySelectorAll('[role=option]')[1].click();
  assert.equal(doc.querySelector('form').checkValidity(), true);
  doc.querySelector('form').reset(); await tick();
  assert.equal(doc.querySelector('.select-value').textContent, 'Choose');
  doc.querySelector('select').disabled = true; await tick();
  assert.equal(doc.querySelector('.select-trigger').disabled, true);
  assert.equal(new w.FormData(doc.querySelector('form')).has('project'), false);
});
