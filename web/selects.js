/* Native selects remain the form data source; all visible menus are rendered here. */
(() => {
  'use strict';
  if (!('showPopover' in HTMLElement.prototype)) return;
  const controls = new Map();
  let active = null, sequence = 0, scheduled = false;
  const make = (tag, className, text) => {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  };
  function labelFor(select) {
    const label = select.labels?.[0];
    return select.getAttribute('aria-label') || (label && [...label.childNodes].filter(n => n.nodeType === 3).map(n => n.textContent).join('').trim()) || (select.name || 'Options').replaceAll('_', ' ');
  }
  function close(restore = false) {
    if (!active) return;
    const control = active;
    active = null;
    if (control.panel.matches(':popover-open')) control.panel.hidePopover();
    control.panel.remove();
    control.button.setAttribute('aria-expanded', 'false');
    if (restore && control.button.isConnected) control.button.focus();
  }
  function position(control) {
    const rect = control.button.getBoundingClientRect(), gap = 7, edge = 12;
    const width = Math.min(Math.max(rect.width, 260), window.innerWidth - edge * 2);
    const below = window.innerHeight - rect.bottom - edge - gap;
    const above = rect.top - edge - gap;
    const up = below < 260 && above > below;
    control.panel.style.width = `${width}px`;
    control.panel.style.maxHeight = `${Math.max(120, Math.min(380, up ? above : below))}px`;
    control.panel.style.left = `${Math.max(edge, Math.min(rect.left, window.innerWidth - width - edge))}px`;
    control.panel.style.top = up ? 'auto' : `${rect.bottom + gap}px`;
    control.panel.style.bottom = up ? `${window.innerHeight - rect.top + gap}px` : 'auto';
  }
  function sync(control) {
    const {select, button} = control;
    const selected = [...select.options].filter(option => option.selected);
    const text = select.multiple ? (selected.length ? selected.slice(0, 2).map(o => o.textContent).join(', ') + (selected.length > 2 ? ` +${selected.length - 2}` : '') : 'Select options') : selected[0]?.textContent || 'Select an option';
    control.value.textContent = text;
    control.value.classList.toggle('is-placeholder', !selected.length);
    button.disabled = select.disabled;
    button.setAttribute('aria-label', `${labelFor(select)}: ${text}`);
    button.setAttribute('aria-required', String(select.required));
    if (select.validity.valid) button.removeAttribute('aria-invalid');
    control.wrapper.classList.toggle('select-multiple', select.multiple);
    if (active === control) {
      if (select.disabled) close();
      else draw(control);
    }
  }
  function highlight(control, index) {
    const choices = control.choices;
    control.highlight = Math.max(0, Math.min(index, choices.length - 1));
    choices.forEach((choice, i) => choice.node.classList.toggle('is-highlighted', i === control.highlight));
    const current = choices[control.highlight];
    if (current) {
      control.search.setAttribute('aria-activedescendant', current.node.id);
      current.node.scrollIntoView({block: 'nearest'});
    } else control.search.removeAttribute('aria-activedescendant');
  }
  function choose(control, option) {
    if (option.disabled || option.parentElement?.disabled) return;
    if (control.select.multiple) option.selected = !option.selected;
    else option.selected = true;
    const multiple = control.select.multiple;
    if (!multiple) close(true);
    // Existing form handlers, including branch updates and sidebar navigation, keep working.
    control.select.dispatchEvent(new Event('input', {bubbles: true}));
    control.select.dispatchEvent(new Event('change', {bubbles: true}));
    if (control.select.isConnected) sync(control);
    if (multiple && active === control) control.search.focus();
  }
  function draw(control) {
    const query = control.search.value.trim().toLocaleLowerCase();
    const previous = control.choices?.[control.highlight]?.option;
    control.list.replaceChildren();
    control.choices = [];
    let group = null;
    for (const option of control.select.options) {
      if (option.hidden || !option.textContent.toLocaleLowerCase().includes(query)) continue;
      const parent = option.parentElement;
      if (parent.tagName === 'OPTGROUP' && parent !== group) {
        control.list.append(make('div', 'select-group', parent.label));
        group = parent;
      }
      const disabled = option.disabled || parent.disabled;
      const row = make('div', 'select-option');
      row.id = `${control.id}-option-${option.index}`;
      row.setAttribute('role', 'option');
      row.setAttribute('aria-selected', String(option.selected));
      row.setAttribute('aria-disabled', String(!!disabled));
      row.append(make('span', 'select-option-label', option.textContent));
      const mark = make('span', 'select-option-mark', option.selected ? '✓' : '');
      mark.setAttribute('aria-hidden', 'true');
      row.append(mark);
      if (!disabled) {
        const index = control.choices.length;
        control.choices.push({node: row, option});
        row.addEventListener('pointermove', () => highlight(control, index));
        row.addEventListener('mousedown', event => event.preventDefault());
        row.addEventListener('click', () => choose(control, option));
      }
      control.list.append(row);
    }
    if (!control.list.children.length) control.list.append(make('div', 'select-empty', query ? 'No matching options' : 'No options available'));
    control.count.textContent = control.select.multiple ? `${[...control.select.options].filter(option => option.selected).length} selected` : `${control.select.options.length} options`;
    const previousIndex = control.choices.findIndex(c => c.option === previous);
    const selectedIndex = control.choices.findIndex(c => c.option.selected);
    highlight(control, previousIndex >= 0 ? previousIndex : Math.max(0, selectedIndex));
  }
  function open(control) {
    if (control.select.disabled) return;
    close();
    active = control;
    const panel = make('div', 'select-panel');
    panel.id = control.id;
    panel.setAttribute('popover', 'manual');
    panel.classList.toggle('select-panel-dark', !!control.select.closest('.sidebar'));
    panel.classList.toggle('select-panel-multiple', control.select.multiple);
    const header = make('div', 'select-panel-header');
    const title = make('span', '', labelFor(control.select));
    control.count = make('span', 'select-count');
    header.append(title, control.count);
    const search = make('input', 'select-search');
    search.type = 'search';
    search.placeholder = 'Search options…';
    search.setAttribute('aria-label', `Search ${labelFor(control.select)}`);
    search.setAttribute('role', 'combobox');
    search.setAttribute('aria-expanded', 'true');
    search.setAttribute('aria-autocomplete', 'list');
    search.setAttribute('aria-controls', `${control.id}-list`);
    const list = make('div', 'select-options');
    list.id = `${control.id}-list`;
    list.setAttribute('role', 'listbox');
    list.setAttribute('aria-label', labelFor(control.select));
    if (control.select.multiple) list.setAttribute('aria-multiselectable', 'true');
    Object.assign(control, {panel, search, list, choices: [], highlight: 0});
    panel.append(header, search, list);
    if (control.select.multiple) {
      const footer = make('div', 'select-panel-footer');
      const clear = make('button', 'select-clear', 'Clear selection');
      clear.type = 'button';
      clear.addEventListener('click', () => {
        for (const option of control.select.options) if (!option.disabled && !option.parentElement.disabled) option.selected = false;
        control.select.dispatchEvent(new Event('change', {bubbles: true}));
        sync(control); search.focus();
      });
      const done = make('button', 'select-done', 'Done');
      done.type = 'button';
      done.addEventListener('click', () => close(true));
      footer.append(clear, done); panel.append(footer);
    }
    (control.select.closest('dialog') || document.body).append(panel);
    panel.showPopover();
    control.button.setAttribute('aria-expanded', 'true');
    position(control); draw(control); search.focus();
    search.addEventListener('input', () => draw(control));
    panel.addEventListener('keydown', event => {
      if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); close(true); }
      else if (event.key === 'Tab') { close(true); }
      else if (event.target === search && ['ArrowDown', 'ArrowUp', 'Enter'].includes(event.key)) {
        event.preventDefault();
        if (event.key === 'Enter') { const item = control.choices[control.highlight]; if (item) choose(control, item.option); }
        else highlight(control, control.highlight + (event.key === 'ArrowDown' ? 1 : -1));
      }
    });
  }
  function enhance(select) {
    const id = `forge-select-${++sequence}`;
    const wrapper = make('span', 'custom-select');
    const button = make('button', 'select-trigger');
    button.type = 'button';
    button.setAttribute('aria-haspopup', 'listbox');
    button.setAttribute('aria-expanded', 'false');
    button.setAttribute('aria-controls', id);
    const value = make('span', 'select-value');
    const chevron = make('span', 'select-chevron');
    chevron.setAttribute('aria-hidden', 'true');
    button.append(value, chevron);
    select.before(wrapper); wrapper.append(select, button);
    select.classList.add('select-source');
    select.tabIndex = -1;
    select.setAttribute('aria-hidden', 'true');
    const control = {id, select, wrapper, button, value};
    controls.set(select, control);
    button.addEventListener('click', () => active === control ? close(true) : open(control));
    button.addEventListener('keydown', event => {
      if (['ArrowDown', 'ArrowUp'].includes(event.key)) { event.preventDefault(); open(control); }
    });
    select.addEventListener('change', () => sync(control));
    select.addEventListener('invalid', event => { event.preventDefault(); button.setAttribute('aria-invalid', 'true'); button.focus(); });
    sync(control);
  }
  function scan() {
    scheduled = false;
    for (const [select, control] of controls) {
      if (!select.isConnected) { if (active === control) close(); controls.delete(select); }
    }
    for (const select of document.querySelectorAll('select')) {
      if (!controls.has(select)) enhance(select);
      const control = controls.get(select);
      const signature = JSON.stringify([select.disabled, select.required, select.multiple, [...select.options].map(o => [o.textContent, o.value, o.selected, o.disabled, o.hidden])]);
      if (signature !== control.signature) { control.signature = signature; sync(control); }
    }
  }
  // Dynamic forms and SSE page updates replace select elements and option lists.
  new MutationObserver(() => { if (!scheduled) { scheduled = true; queueMicrotask(scan); } }).observe(document.body, {childList: true, subtree: true, attributes: true, attributeFilter: ['disabled', 'selected', 'required', 'multiple']});
  document.addEventListener('pointerdown', event => { if (active && !active.wrapper.contains(event.target) && !active.panel.contains(event.target)) close(); });
  document.addEventListener('close', event => { if (active && event.target.contains(active.select)) close(); }, true);
  document.addEventListener('reset', () => queueMicrotask(() => { for (const control of controls.values()) sync(control); }));
  window.addEventListener('resize', () => { if (active) position(active); });
  document.addEventListener('scroll', event => { if (active && !active.panel.contains(event.target)) position(active); }, true);
  scan();
})();
