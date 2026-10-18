const { getDefaultConfig } = require('expo/metro-config');

const config = getDefaultConfig(__dirname);

// mermaid 的源码作为**文本资源**随包发：WebView 里内联执行它，走 Metro 的模块系统
// 会被当成普通 JS 模块要求去执行/导出，所以让它停在 assets 这一层。
config.resolver.assetExts.push('txt');

module.exports = config;
