'use strict';

// app.js 测试辅助：读取资源目录并以无 DOM 环境加载纯逻辑。
const fs = require('node:fs');
const path = require('node:path');

const ASSETS_DIR = process.env.CM_ASSETS_DIR
  ? path.resolve(process.env.CM_ASSETS_DIR)
  : path.resolve(__dirname, '..', '..', 'internal', 'web', 'assets');

function readAsset(name) {
  return fs.readFileSync(path.join(ASSETS_DIR, name), 'utf8');
}

// 以无 DOM 的宿主 realm 执行 app.js：document 为 undefined，因此不会触发 boot，
// 同时保证断言可以严格比较（同 realm 原型一致）。
function loadCM() {
  const source = readAsset('app.js');
  const factory = new Function('window', 'document', source + '\n//# sourceURL=app.js');
  const win = {};
  factory(win, undefined);
  if (!win.CM) throw new Error('app.js 未导出 window.CM');
  return win.CM;
}

module.exports = { ASSETS_DIR, readAsset, loadCM };
