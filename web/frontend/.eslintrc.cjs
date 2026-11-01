// 这份配置**曾经不存在**：`package.json` 里的 `lint` 脚本和 eslint / @typescript-eslint /
// react-hooks 三个插件一直都在，但没有任何配置文件，于是 `npm run lint` 一跑就死在
// 「couldn't find a configuration file」。它坏着也没人知道 —— 因为没有任何地方跑过它
// （`make lint` 只跑 golangci-lint）。现在它由 `make web-lint` 跑，CI 的 test job 也跑。
module.exports = {
  root: true,
  env: { browser: true, es2022: true },
  parser: '@typescript-eslint/parser',
  parserOptions: { ecmaVersion: 'latest', sourceType: 'module', ecmaFeatures: { jsx: true } },
  plugins: ['@typescript-eslint', 'react-hooks'],
  // react-hooks 是这套里最值钱的两条：条件式调用 hook、依赖数组写错的那些 bug，
  // 类型检查看不见，运行时只表现为「某个值不更新」。
  extends: ['eslint:recommended', 'plugin:@typescript-eslint/recommended', 'plugin:react-hooks/recommended'],
  ignorePatterns: ['dist', 'node_modules'],
  rules: {
    // 下划线前缀 = 「这个绑定是故意不用的」，最常见的形状是解构时摘掉某个键：
    // `const { type: _type, ...req } = e` —— 删掉它反而是错的（那个键会留在 req 里）。
    '@typescript-eslint/no-unused-vars': [
      'error',
      { argsIgnorePattern: '^_', varsIgnorePattern: '^_', caughtErrorsIgnorePattern: '^_' },
    ],
  },
}
