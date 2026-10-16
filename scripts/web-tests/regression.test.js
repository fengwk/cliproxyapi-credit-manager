'use strict';

// 管理界面缺陷的回归测试（修复前全部失败）：
// 快照状态标签必须覆盖服务端真实取值、非法美元输入不得沿用旧的微美元残留值、
// 自定义密钥材料必须在显式提供时原样下发且绝不出现在更新请求里。
const test = require('node:test');
const assert = require('node:assert/strict');
const { loadCM } = require('./helpers');

const CM = loadCM();

test('captureStatusText 覆盖服务端真实状态并保持未知状态安全', () => {
  // 取值来自 internal/capture 的 Snapshot 状态常量，界面必须逐个给出中文标签。
  assert.deepEqual(CM.captureStatusText('captured'), { label: '快照可用', tone: 'ok' });
  assert.deepEqual(CM.captureStatusText('disabled'), { label: '捕获功能未启用', tone: 'warn' });
  assert.deepEqual(CM.captureStatusText('dropped'), { label: '快照已丢弃', tone: 'warn' });
  assert.deepEqual(CM.captureStatusText('oversize'), { label: '快照超出体积上限', tone: 'warn' });
  assert.deepEqual(CM.captureStatusText('expired'), { label: '快照已过期', tone: 'warn' });
  assert.deepEqual(CM.captureStatusText('unavailable'), { label: '快照不可用', tone: 'warn' });
  // 未收录状态原样显示，不猜测、不报错。
  assert.deepEqual(CM.captureStatusText('weird'), { label: 'weird', tone: '' });
  assert.deepEqual(CM.captureStatusText(''), { label: '未知状态', tone: '' });
  assert.deepEqual(CM.captureStatusText(undefined), { label: '未知状态', tone: '' });
});

test('moneyInputMicro 拒绝非法美元输入而不是沿用旧的微美元残留值', () => {
  // 先有合法的微美元值（用户在美元框里输入 1 之后又改成非法金额）：
  // 必须报错并且绝不把旧的 1000000 当成提交金额。
  const stale = [];
  assert.equal(CM.moneyInputMicro({ micro: '1234567', usd: '0.0000001' }, '总额度', stale), '0');
  assert.equal(stale.length, 1);
  assert.ok(stale[0].includes('美元输入无效'), stale[0]);
  assert.ok(stale[0].includes('总额度'), stale[0]);

  const garbage = [];
  assert.equal(CM.moneyInputMicro({ micro: '999999', usd: 'garbage' }, '金额', garbage), '0');
  assert.equal(garbage.length, 1);
  assert.ok(garbage[0].includes('美元输入无效'), garbage[0]);

  // 合法美元输入以美元字段为准精确换算（纯换算行为不变）。
  assert.equal(CM.moneyInputMicro({ micro: '1', usd: '1.25' }, '金额', []), '1250000');
  assert.equal(CM.moneyInputMicro({ micro: '1', usd: '0.000001' }, '金额', []), '1');

  // 两个字段都为空时才表示 0（留空 = 0）。
  assert.equal(CM.moneyInputMicro({ micro: '', usd: '' }, '金额', []), '0');
  assert.equal(CM.moneyInputMicro({ micro: '  ', usd: '  ' }, '金额', []), '0');

  // 非法微美元输入仍然报错，绝不静默变成 0/不限/免费。
  const microText = [];
  assert.equal(CM.moneyInputMicro({ micro: 'abc', usd: '' }, '金额', microText), '0');
  assert.ok(microText[0].includes('整数微美元'), microText[0]);
  const negative = [];
  assert.equal(CM.moneyInputMicro({ micro: '-5', usd: '' }, '金额', negative), '0');
  assert.ok(negative[0].includes('不能为负数'), negative[0]);
});

test('keySpecPayload 只在显式提供时原样下发自定义密钥材料', () => {
  const base = {
    label: 'x',
    enabled: true,
    quotaMicroUSD: '0',
    dailyMicroUSD: '0',
    weeklyMicroUSD: '0',
    monthlyMicroUSD: '0',
    maxConcurrent: '0',
    allowedModels: [],
  };
  const generated = CM.keySpecPayload(base);
  assert.equal(Object.prototype.hasOwnProperty.call(generated, 'key'), false, '留空必须由服务端生成，不能下发空 key');

  // 自定义材料必须原样下发（不裁剪），首尾空白交由服务端校验拒绝。
  const custom = CM.keySpecPayload(Object.assign({}, base, { key: '  sk-raw  ' }));
  assert.equal(custom.key, '  sk-raw  ');
  assert.equal(CM.exactStringify(custom).includes('"key":"  sk-raw  "'), true);

  // 更新路径必须剔除 key 材料，否则 PUT 会因为回传材料被服务端拒绝。
  assert.equal('key' in CM.mergeKeySpec({ key: 'sk-old', label: 'a' }, custom), false);
});
