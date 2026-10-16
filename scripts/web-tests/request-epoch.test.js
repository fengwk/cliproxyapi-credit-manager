'use strict';

// request() 的会话纪元边界：fetch 已经兑现但响应体读取失败（response.text() 被拒绝）时，
// 也必须按发起请求时的会话纪元判定是否过期——与网络失败、HTTP 错误一视同仁。
// 否则登出/重新登录之后到达的普通错误会被当成新会话的错误写入界面状态。
const test = require('node:test');
const assert = require('node:assert/strict');
const { loadCM } = require('./helpers');

// 受控 fetch：立即兑现一个响应，但把响应体读取的拒绝时机交给测试控制。
function stubDeferredBodyRejection() {
  const realFetch = global.fetch;
  let rejectBody = null;
  global.fetch = () =>
    Promise.resolve({
      ok: true,
      status: 200,
      text: () =>
        new Promise((resolve, reject) => {
          rejectBody = reject;
        }),
    });
  return {
    // request() 的 fetch/text 调用发生在微任务里，先排空微任务再拒绝，避免依赖墙钟等待。
    async reject(message) {
      for (let i = 0; i < 10 && !rejectBody; i += 1) await Promise.resolve();
      assert.ok(rejectBody, '响应体读取尚未开始，无法模拟读取失败');
      rejectBody(new Error(message));
    },
    restore() {
      global.fetch = realFetch;
    },
  };
}

test('响应体读取失败时按发起时的会话纪元作废（旧会话 → kind=stale）', async () => {
  const CM = loadCM();
  const stub = stubDeferredBodyRejection();
  try {
    const epoch = CM.state.sessionEpoch;
    const pending = CM.request('/v0/management/plugins/credit-manager/api/usage');
    // 会话变更（登出或重新登录）：旧会话的响应体读取失败必须作废，
    // 因此调用方（failRequest）会静默丢弃，绝不写进新会话的错误/弹窗状态。
    CM.state.sessionEpoch = epoch + 1;
    await stub.reject('body stream aborted');
    await assert.rejects(pending, (error) => error.kind === 'stale');
  } finally {
    stub.restore();
  }
});

test('响应体读取失败在当前会话内保留原始诊断信息', async () => {
  const CM = loadCM();
  const stub = stubDeferredBodyRejection();
  try {
    const pending = CM.request('/v0/management/plugins/credit-manager/api/usage');
    // 会话未变更：读取失败必须原样上抛（不是 stale），界面才能给出真实的失败原因。
    await stub.reject('body stream aborted');
    await assert.rejects(pending, (error) => {
      assert.notEqual(error.kind, 'stale');
      assert.match(String(error.message), /body stream aborted/);
      return true;
    });
  } finally {
    stub.restore();
  }
});
