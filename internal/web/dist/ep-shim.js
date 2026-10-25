/* 公开状态页的 Element Plus 垫片（v1.28.0 起）。
 *
 * 为什么存在：公开页此前加载 Element Plus 全量包（JS 1MB + CSS 361KB + dark.css），
 * 而实际用到的组件只有十几个、每个只用了两三个 props。这个文件以 Vue 函数式
 * 组件按**同名接口**补齐它们（填充 window.ElementPlus），status.js 不需要任何改动。
 *
 * 设计约束：
 *   - 只实现 status.js 实际用到的 props/slots，未实现的显式忽略；
 *     组件全部基于 --app-* 变量取色，深浅色随 html.dark 自动切换。
 *   - ElRadioGroup/ElRadioButton 用 provide/inject 传递选中态；
 *     ElSelect/ElOption 退化为原生 <select>（对本页的分组筛选是更好的控件）。
 *   - install 为空操作：status.js 的 app.use(EP, {locale}) 只是注册入口。
 */
(function () {
  'use strict';
  var Vue = window.Vue;
  if (!Vue || window.ElementPlus) return; // Vue 缺失时保留 CSS 兜底提示；避免重复注入

  var h = Vue.h;

  function merge(props, attrs) {
    var out = {};
    var k;
    for (k in (props || {})) out[k] = props[k];
    for (k in (attrs || {})) if (!(k in out)) out[k] = attrs[k];
    return out;
  }
  function slot(children, name) {
    return children && children[name] ? children[name]() : (children && children.default ? children.default() : null);
  }

  var ElCard = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    var head = ctx.slots && ctx.slots.header ? ctx.slots.header() : null;
    var body = slot(ctx.slots, 'default');
    return h('div', { class: 'ep-card' + (p.class ? ' ' + p.class : '') }, [
      head ? h('div', { class: 'ep-card__header' }, head) : null,
      h('div', { class: 'ep-card__body' }, body)
    ]);
  };

  var ElTag = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    return h('span', {
      class: 'ep-tag ep-tag--' + (p.type || 'info') + (p.round ? ' ep-tag--round' : ''),
      onClick: p.onClick
    }, slot(ctx.slots, 'default'));
  };

  var ElButton = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    return h(p.link ? 'a' : 'button', {
      class: 'ep-btn ep-btn--' + (p.type || 'default') + (p.link ? ' ep-btn--link' : '') +
        (p.size === 'small' ? ' ep-btn--sm' : '') +
        (p.circle ? ' ep-btn--circle' : '') + (p.plain ? ' ep-btn--plain' : ''),
      disabled: p.disabled,
      type: p.link ? null : 'button',
      title: p.title,
      'aria-label': p['aria-label'] || null,
      onClick: p.onClick,
      href: p.link ? 'javascript:void(0)' : null
    }, slot(ctx.slots, 'default'));
  };

  var ElAlert = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    return h('div', { class: 'ep-alert ep-alert--' + (p.type || 'info') + (p.class ? ' ' + p.class : ''), role: 'alert' }, [
      h('span', { class: 'ep-alert__icon', 'aria-hidden': 'true' },
        p.type === 'error' ? '✕' : p.type === 'warning' ? '!' : 'i'),
      h('div', { class: 'ep-alert__body' }, [
        h('div', { class: 'ep-alert__title' }, p.title),
        p.description ? h('div', { class: 'ep-alert__desc' }, p.description) : null
      ])
    ]);
  };

  var ElProgress = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    var pct = Math.max(0, Math.min(100, Number(p.percentage) || 0));
    var color = p.color || 'var(--app-accent)';
    if (p.type === 'circle') {
      var size = p.width || 88, sw = p.strokeWidth || 6, r = (size - sw) / 2, c = 2 * Math.PI * r;
      var text = p['show-text'] && p.format ? p.format(pct) : Math.round(pct) + '%';
      return h('div', { class: 'ep-ring', style: 'width:' + size + 'px;height:' + size + 'px' }, [
        h('svg', { width: size, height: size, viewBox: '0 0 ' + size + ' ' + size }, [
          h('circle', { cx: size / 2, cy: size / 2, r: r, fill: 'none', stroke: 'var(--app-line)', 'stroke-width': sw }),
          h('circle', {
            cx: size / 2, cy: size / 2, r: r, fill: 'none', stroke: color, 'stroke-width': sw,
            'stroke-linecap': 'round', 'stroke-dasharray': c.toFixed(1),
            'stroke-dashoffset': (c * (1 - pct / 100)).toFixed(1),
            transform: 'rotate(-90 ' + size / 2 + ' ' + size / 2 + ')'
          })
        ]),
        text ? h('div', { class: 'ep-ring__text' }, text) : null
      ]);
    }
    return h('div', { class: 'ep-progress' }, [
      h('i', { style: 'width:' + pct + '%;background:' + color })
    ]);
  };

  var ElEmpty = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    return h('div', { class: 'ep-empty' }, [
      h('div', { class: 'ep-empty__icon', 'aria-hidden': 'true' }, '·'),
      h('div', { class: 'ep-empty__text' }, p.description || '暂无数据')
    ]);
  };

  var ElDescriptions = function (props, ctx) {
    return h('div', { class: 'ep-desc' + (props.class ? ' ' + props.class : '') }, slot(ctx.slots, 'default'));
  };
  var ElDescriptionsItem = function (props, ctx) {
    return h('div', { class: 'ep-desc-row' }, [
      h('span', { class: 'ep-desc-label' }, props.label),
      h('span', { class: 'ep-desc-value' }, slot(ctx.slots, 'default'))
    ]);
  };

  var ElTimeline = function (props, ctx) {
    return h('div', { class: 'ep-timeline' }, slot(ctx.slots, 'default'));
  };
  var ElTimelineItem = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    return h('div', { class: 'ep-tl-item ep-tl--' + (p.type || 'info') + (p.hollow ? ' ep-tl--hollow' : '') }, [
      h('div', { class: 'ep-tl-side' }, [
        h('span', { class: 'ep-tl-dot' }),
        p.timestamp ? h('span', { class: 'ep-tl-time' }, p.timestamp) : null
      ]),
      h('div', { class: 'ep-tl-body' }, slot(ctx.slots, 'default'))
    ]);
  };

  var ElResult = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    return h('div', { class: 'ep-result ep-result--' + (p.icon || 'info') }, [
      h('div', { class: 'ep-result__icon', 'aria-hidden': 'true' }, p.icon === 'success' ? '✓' : p.icon === 'warning' ? '!' : 'i'),
      h('div', { class: 'ep-result__title' }, p.title),
      p.subTitle ? h('div', { class: 'ep-result__sub' }, p.subTitle) : null,
      ctx.slots && ctx.slots.default ? h('div', { class: 'ep-result__extra' }, ctx.slots.default()) : null
    ]);
  };

  var ElPagination = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    var pages = Math.max(1, Math.ceil((Number(p.total) || 0) / (Number(p.pageSize) || 1)));
    var cur = Number(p.currentPage) || 1;
    var change = p['onCurrentChange'];
    return h('div', { class: 'ep-pager' }, [
      h('button', { class: 'ep-pager__btn', disabled: cur <= 1, onClick: function () { change && change(cur - 1); } }, '‹'),
      h('span', { class: 'ep-pager__cur' }, cur + ' / ' + pages),
      h('button', { class: 'ep-pager__btn', disabled: cur >= pages, onClick: function () { change && change(cur + 1); } }, '›')
    ]);
  };

  // 单选组：provide/inject 在组与按钮之间传递选中态与更新回调。
  var RADIO_KEY = 'epRadio';
  var ElRadioGroup = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    return h(Vue.defineComponent({
      setup: function () {
        Vue.provide(RADIO_KEY, {
          value: function () { return p.modelValue; },
          update: function (v) { p['onUpdate:modelValue'] && p['onUpdate:modelValue'](v); }
        });
        return function () { return h('div', { class: 'ep-radio' + (p.class ? ' ' + p.class : ''), role: 'radiogroup' }, slot(ctx.slots, 'default')); };
      }
    }));
  };
  var ElRadioButton = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    return h(Vue.defineComponent({
      setup: function () {
        var group = Vue.inject(RADIO_KEY, null);
        return function () {
          var on = group && String(group.value()) === String(p.value);
          return h('button', {
            class: 'ep-radio-btn' + (on ? ' on' : ''),
            role: 'radio', 'aria-checked': on ? 'true' : 'false',
            onClick: function () { group && group.update(p.value); }
          }, slot(ctx.slots, 'default'));
        };
      }
    }));
  };

  var ElInput = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    return h(Vue.defineComponent({
      setup: function () {
        var v = Vue.ref(p.modelValue);
        Vue.watch(function () { return p.modelValue; }, function (n) { v.value = n; });
        return function () {
          return h('span', { class: 'ep-input' + (p.class ? ' ' + p.class : '') }, [
            ctx.slots && ctx.slots.prefix ? h('span', { class: 'ep-input__prefix' }, ctx.slots.prefix()) : null,
            h('input', {
              value: v.value,
              placeholder: p.placeholder || '',
              'aria-label': p['aria-label'] || null,
              onInput: function (e) { v.value = e.target.value; p['onUpdate:modelValue'] && p['onUpdate:modelValue'](e.target.value); }
            }),
            p.clearable && v.value
              ? h('span', {
                  class: 'ep-input__clear', role: 'button', 'aria-label': '清空',
                  onClick: function () { v.value = ''; p['onUpdate:modelValue'] && p['onUpdate:modelValue'](''); }
                }, '×')
              : null
          ]);
        };
      }
    }));
  };

  var ElSelect = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    return h('select', {
      class: 'ep-select' + (p.class ? ' ' + p.class : ''),
      onChange: function (e) { p['onUpdate:modelValue'] && p['onUpdate:modelValue'](e.target.value); }
    }, slot(ctx.slots, 'default'));
  };
  var ElOption = function (props, ctx) {
    var p = merge(props, ctx.attrs);
    return h('option', { value: p.value }, p.label != null ? p.label : slot(ctx.slots, 'default'));
  };

  var ElConfigProvider = function (props, ctx) { return slot(ctx.slots, 'default'); };

  var messageTimer = null;
  function ElMessage(opts) {
    if (typeof opts === 'string') opts = { message: opts };
    var old = document.querySelector('.ep-toast');
    if (old) old.remove();
    if (messageTimer) clearTimeout(messageTimer);
    var el = document.createElement('div');
    el.className = 'ep-toast ep-toast--' + (opts.type || 'info');
    el.setAttribute('role', 'status');
    el.textContent = opts.message || '';
    document.body.appendChild(el);
    messageTimer = setTimeout(function () { el.remove(); }, opts.duration || 2400);
  }

  window.ElementPlus = {
    install: function () {},
    ElCard: ElCard, ElTag: ElTag, ElButton: ElButton, ElAlert: ElAlert,
    ElProgress: ElProgress, ElEmpty: ElEmpty, ElDescriptions: ElDescriptions,
    ElDescriptionsItem: ElDescriptionsItem, ElTimeline: ElTimeline,
    ElTimelineItem: ElTimelineItem, ElResult: ElResult, ElPagination: ElPagination,
    ElRadioGroup: ElRadioGroup, ElRadioButton: ElRadioButton,
    ElInput: ElInput, ElSelect: ElSelect, ElOption: ElOption,
    ElConfigProvider: ElConfigProvider, ElMessage: ElMessage
  };
})();
