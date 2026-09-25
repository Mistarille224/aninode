import {translate, formatMessage} from "./i18n.js";
// Keep native form values, labels and validation; only replace platform chrome.
export function installControls(root = document) {
  let menu, owner, active = -1, typed = '', typedAt = 0;
  const close = () => {
    if (!owner) return;
    owner.setAttribute('aria-expanded', 'false');
    owner.removeAttribute('aria-controls');
    owner.removeAttribute('aria-activedescendant');
    menu.remove();
    owner = menu = null;
  };
  const enabled = option => option && !option.disabled && !option.parentElement.disabled && !option.hidden;
  const highlight = index => {
    active = index;
    for (const row of menu.children) row.classList.toggle('active', Number(row.dataset.index) === index);
    const row = menu.querySelector(`[data-index="${index}"]`);
    if (row) {
      owner.setAttribute('aria-activedescendant', row.id);
      row.scrollIntoView({block: 'nearest'});
    }
  };
  const choose = () => {
    const select = owner;
    if (!select || !enabled(select.options[active])) return;
    const changed = select.selectedIndex !== active;
    select.selectedIndex = active;
    close();
    select.focus();
    if (changed) {
      select.dispatchEvent(new Event('input', {bubbles: true}));
      select.dispatchEvent(new Event('change', {bubbles: true}));
    }
  };
  const open = select => {
    close();
    owner = select;
    typed = '';
    menu = document.createElement('div');
    menu.className = 'select-menu';
    menu.id = 'control-options';
    menu.setAttribute('role', 'listbox');
    menu.setAttribute('aria-label', select.getAttribute('aria-label') || select.labels?.[0]?.querySelector('span')?.textContent || translate('Options'));
    [...select.options].forEach((option, index) => {
      if (option.hidden) return;
      const row = document.createElement('div');
      row.id = `control-option-${index}`;
      row.dataset.index = index;
      row.setAttribute('role', 'option');
      row.setAttribute('aria-selected', String(option.selected));
      row.setAttribute('aria-disabled', String(!enabled(option)));
      row.textContent = option.textContent;
      row.addEventListener('pointerdown', event => event.preventDefault());
      row.addEventListener('click', () => { if (enabled(option)) { active = index; choose(); } });
      menu.append(row);
    });
    (select.closest('.modal') || document.body).append(menu);
    const rect = select.getBoundingClientRect();
    menu.style.width = `${Math.min(rect.width, innerWidth - 16)}px`;
    menu.style.left = `${Math.max(8, Math.min(rect.left, innerWidth - menu.offsetWidth - 8))}px`;
    const below = innerHeight - rect.bottom - 8, above = rect.top - 8;
    const upward = below < Math.min(240, menu.scrollHeight) && above > below;
    menu.style.maxHeight = `${Math.max(0, Math.min(280, upward ? above : below))}px`;
    menu.style.top = `${upward ? rect.top - menu.offsetHeight - 4 : rect.bottom + 4}px`;
    select.setAttribute('aria-expanded', 'true');
    select.setAttribute('aria-controls', menu.id);
    select.focus();
    highlight(select.selectedIndex);
  };
  const isSelect = node => node?.matches?.('select:not([multiple])') && node.size <= 1 && !node.disabled;
  // Cancel pointerdown as well as click: desktop browsers open native pickers early.
  root.addEventListener('pointerdown', event => {
    if (isSelect(event.target)) event.preventDefault();
    else if (owner && !menu.contains(event.target)) close();
  }, true);
  root.addEventListener('click', event => {
    if (!isSelect(event.target)) return;
    event.preventDefault();
    owner === event.target ? close() : open(event.target);
  }, true);
  root.addEventListener('keydown', event => {
    if (!isSelect(event.target)) return;
    const select = event.target, key = event.key;
    if (key === 'Tab') { close(); return; }
    if (key === 'Escape') {
      if (owner) { event.preventDefault(); event.stopImmediatePropagation(); close(); }
      return;
    }
    if (event.metaKey || event.ctrlKey || (event.altKey && key !== 'ArrowDown')) return;
    if (!['ArrowDown', 'ArrowUp', 'Home', 'End', 'Enter', ' '].includes(key) && key.length !== 1) return;
    event.preventDefault();
    const wasOpen = owner === select;
    if (!wasOpen) open(select);
    if (key === 'Enter' || key === ' ') { if (wasOpen) choose(); return; }
    const indices = [...select.options].flatMap((option, index) => enabled(option) ? [index] : []);
    if (!indices.length) return;
    let index = indices.indexOf(active);
    if (key === 'Home') index = 0;
    else if (key === 'End') index = indices.length - 1;
    else if (key === 'ArrowDown') index = Math.min(indices.length - 1, index + 1);
    else if (key === 'ArrowUp') index = Math.max(0, index - 1);
    else {
      const now = Date.now();
      typed = now - typedAt > 700 ? key : typed + key;
      typedAt = now;
      const match = indices.find(i => select.options[i].textContent.trim().toLocaleLowerCase().startsWith(typed.toLocaleLowerCase()));
      if (match !== undefined) highlight(match);
      return;
    }
    highlight(indices[index]);
  }, true);
  root.addEventListener('focusin', event => { if (owner && event.target !== owner) close(); });
  root.addEventListener('scroll', event => { if (owner && event.target !== menu) close(); }, true);
  window.addEventListener('resize', close);
  window.addEventListener('blur', close);
  root.addEventListener('change', event => { if (event.target === owner) close(); });

  const enhanceControls = () => {
    if (owner && (!owner.isConnected || owner.disabled)) close();
    for (const select of root.querySelectorAll('select:not([multiple])')) {
      if (select.size > 1 || select.parentElement.classList.contains('select-control')) continue;
      const wrapper = document.createElement('span');
      wrapper.className = 'select-control';
      select.before(wrapper);
      wrapper.append(select);
    }
    for (const input of root.querySelectorAll('input[type="number"]')) {
      if (input.parentElement.classList.contains('number-control')) continue;
      const wrapper = document.createElement('span');
      wrapper.className = 'number-control';
      input.before(wrapper);
      wrapper.append(input);
      const steps = document.createElement('span');
      steps.className = 'number-steps';
      for (const [direction, label] of [[1, 'Increase'], [-1, 'Decrease']]) {
        const button = document.createElement('button');
        button.type = 'button';
        button.className = direction === 1 ? 'step-up' : 'step-down';
        button.setAttribute('aria-label', formatMessage(direction === 1 ? 'Increase {value}' : 'Decrease {value}', {value: input.getAttribute('aria-label') || input.labels?.[0]?.querySelector('span')?.textContent || translate('value')}));
        button.disabled = input.disabled || input.readOnly;
        button.addEventListener('click', event => {
          event.preventDefault();
          if (input.disabled || input.readOnly) return;
          const before = input.value;
          // Native stepping preserves min/max, fractional steps and empty values.
          if (input.step === 'any') return;
          direction === 1 ? input.stepUp() : input.stepDown();
          input.focus();
          if (input.value !== before) {
            input.dispatchEvent(new Event('input', {bubbles: true}));
            input.dispatchEvent(new Event('change', {bubbles: true}));
          }
        });
        steps.append(button);
      }
      wrapper.append(steps);
    }
    for (const button of root.querySelectorAll('.number-steps button')) {
      const input = button.closest('.number-control').querySelector('input');
      const disabled = input.disabled || input.readOnly || input.step === 'any';
      if (button.disabled !== disabled) button.disabled = disabled;
    }
  };
  enhanceControls();
  // Rendering replaces controls, including rows added inside an open editor.
  new MutationObserver(enhanceControls).observe(root.body || root, {subtree: true, childList: true, attributes: true, attributeFilter: ['disabled', 'readonly', 'step']});
}
