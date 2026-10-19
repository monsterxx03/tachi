import MarkdownView, {
  renderRules as defaultRules,
  type MarkdownStyleMap,
  type RenderRules,
} from '@ronradtke/react-native-markdown-display';
import { useMemo } from 'react';
import { StyleSheet, Text as RNText, type TextProps } from 'react-native';
import { useTheme } from '../theme/ThemeContext';
import type { ThemeTokens } from '../theme/tokens';
import { Mermaid } from './Mermaid';

/**
 * 正文的 markdown 交给成熟库渲染（markdown-it 解析，表格 / 列表 / 代码块都是它自己的规则），
 * 我们自己只补两件事：
 *
 *   1. 围栏里的 `mermaid` 换成 WebView 那张图（库的其余围栏照旧，自带复制按钮）。
 *   2. 所有文字都能长按选中 —— 手机上没有别的办法复制 agent 的输出。
 */
function SelectableText(props: TextProps) {
  return <RNText selectable {...props} />;
}

function rulesOf(): RenderRules {
  // 库的默认规则是工厂：先给它那个可长按选中的 Text，mermaid 之外照旧用它。
  const base = defaultRules(SelectableText);
  return {
    fence: (node, children, parent, styles, onCopyCode, colorScheme) => {
      const lang =
        typeof node.sourceInfo === 'string' ? (node.sourceInfo.trim().split(/\s+/)[0] ?? '') : '';
      if (lang === 'mermaid') return <Mermaid key={node.key} source={node.content.trim()} />;
      return base.fence!(node, children, parent, styles, onCopyCode, colorScheme);
    },
  };
}

function styleOf(t: ThemeTokens, color: string): MarkdownStyleMap {
  const mono = { fontFamily: 'monospace' };
  const heading = (size: number) => ({
    color: t.title,
    fontSize: size,
    fontWeight: '700' as const,
    marginTop: 12,
    marginBottom: 5,
  });
  return {
    body: { color, fontSize: 15, lineHeight: 25 },
    text: { color },
    paragraph: {
      marginTop: 4,
      marginBottom: 4,
      flexWrap: 'wrap',
      flexDirection: 'row',
      alignItems: 'flex-start',
      width: '100%',
    },
    heading1: heading(19),
    heading2: heading(17.5),
    heading3: heading(16.5),
    heading4: heading(15.5),
    heading5: heading(15),
    heading6: heading(15),
    strong: { fontWeight: '700' },
    em: { fontStyle: 'italic' },
    s: { textDecorationLine: 'line-through' },
    link: { color: t.accent2, textDecorationLine: 'underline' },
    hr: { backgroundColor: t.border, height: StyleSheet.hairlineWidth, marginVertical: 12 },
    blockquote: {
      backgroundColor: 'transparent',
      borderColor: t.borderStrong,
      borderLeftWidth: 2,
      marginLeft: 0,
      paddingHorizontal: 10,
      marginVertical: 7,
    },
    bullet_list_icon: { color: t.faint, marginLeft: 8, marginRight: 8 },
    ordered_list_icon: { color: t.faint, marginLeft: 8, marginRight: 8 },
    code_inline: {
      color: t.codeInk,
      backgroundColor: t.codeBg,
      paddingHorizontal: 4,
      paddingVertical: 1,
      borderRadius: 4,
      borderWidth: 0,
      ...mono,
    },
    fence: {
      borderColor: t.border,
      borderWidth: StyleSheet.hairlineWidth,
      borderRadius: 8,
      backgroundColor: t.codeBg,
      marginVertical: 8,
    },
    fence_header: {
      backgroundColor: t.wash,
      borderBottomColor: t.border,
      borderBottomWidth: StyleSheet.hairlineWidth,
      paddingHorizontal: 10,
      paddingVertical: 4,
    },
    fence_language_label: { color: t.faint, fontSize: 11, ...mono },
    fence_copy_text: { color: t.faint, fontSize: 11 },
    fence_code: { backgroundColor: t.codeBg, padding: 10 },
    fence_token: { color: t.codeInk, fontSize: 12.5, lineHeight: 19, ...mono },
    // 表格：等宽的列 + 细网格。默认样式没有竖线，窄屏上两列会黏在一起。
    table: {
      borderColor: t.border,
      borderWidth: StyleSheet.hairlineWidth,
      borderRadius: 8,
      marginVertical: 8,
    },
    tr: {
      borderBottomColor: t.border,
      borderBottomWidth: StyleSheet.hairlineWidth,
      flexDirection: 'row',
    },
    th: {
      flex: 1,
      paddingVertical: 6,
      paddingHorizontal: 10,
      borderRightColor: t.border,
      borderRightWidth: StyleSheet.hairlineWidth,
      backgroundColor: t.wash,
    },
    td: {
      flex: 1,
      paddingVertical: 6,
      paddingHorizontal: 10,
      borderRightColor: t.border,
      borderRightWidth: StyleSheet.hairlineWidth,
    },
  };
}

export function Markdown({ text, ink }: { text: string; ink?: string }) {
  const { tokens: t } = useTheme();
  const color = ink ?? t.text;
  const rules = useMemo(() => rulesOf(), []);
  const style = useMemo(() => styleOf(t, color), [t, color]);

  return (
    <MarkdownView style={style} rules={rules} textcomponent={SelectableText} mergeStyle>
      {text}
    </MarkdownView>
  );
}
