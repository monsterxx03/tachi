/**
 * 与桌面同一份配色 token。
 *
 * 值来自 desktop/frontend/public/base.css（深色是 Ayu Mirage 那套石墨色，
 * 浅色是暖纸 + 墨蓝）。这里只抄手机用得上的那部分，与设计预览
 * docs/2026-10-18-mobile-prototype.html 里的 :root 一致。
 *
 * 规矩：不在这里、也不在组件里散落新的硬编码颜色 —— 要加颜色先进 base.css
 * 的 token，再同步过来。
 */
export interface ThemeTokens {
  bg: string;
  bgTop: string;
  panel: string;
  panel2: string;
  raise: string;
  border: string;
  borderStrong: string;
  wash: string;
  washStrong: string;
  text: string;
  dim: string;
  faint: string;
  title: string;
  accent: string;
  accent2: string;
  accentStrong: string;
  accentSoft: string;
  accentLine: string;
  onAccent: string;
  green: string;
  red: string;
  redWash: string;
  redLine: string;
  amber: string;
  amberWash: string;
  amberLine: string;
  /** 用户气泡：一层浅浅的底色 + 深色墨水，不是饱和的蓝色填充 */
  userBg: string;
  userInk: string;
  codeBg: string;
  codeInk: string;
  cmdBg: string;
  cmdInk: string;
  inputBg: string;
  composerBg: string;
  stripBg: string;
  headBg: string;
  toastBg: string;
  switchOff: string;
  switchKnob: string;
  /** 遮罩层（sheet / 阅读器背后的那层） */
  scrim: string;
}

export const dark: ThemeTokens = {
  bg: '#252a35',
  bgTop: '#292e3a',
  panel: '#303541',
  panel2: '#343944',
  raise: '#2b303b',
  border: 'rgba(255,255,255,0.09)',
  borderStrong: 'rgba(255,255,255,0.14)',
  wash: 'rgba(255,255,255,0.045)',
  washStrong: 'rgba(255,255,255,0.07)',
  text: '#cecdc6',
  dim: '#a9b0bd',
  faint: '#9ca3b1',
  title: '#e4e3dc',
  accent: '#7c96ff',
  accent2: '#9db1ff',
  accentStrong: '#4c589c',
  accentSoft: 'rgba(124,150,255,0.16)',
  accentLine: 'rgba(124,150,255,0.48)',
  onAccent: '#f2f4ff',
  green: '#46c98a',
  red: '#ff7a82',
  redWash: 'rgba(255,122,130,0.14)',
  redLine: 'rgba(255,122,130,0.45)',
  amber: '#e2a355',
  amberWash: 'rgba(226,163,85,0.13)',
  amberLine: 'rgba(226,163,85,0.35)',
  userBg: '#2c3246',
  userInk: '#dde3ff',
  codeBg: 'rgba(255,255,255,0.08)',
  codeInk: '#e6e4dc',
  cmdBg: 'rgba(0,0,0,0.24)',
  cmdInk: '#e8e6df',
  inputBg: 'rgba(0,0,0,0.16)',
  composerBg: 'rgba(0,0,0,0.12)',
  stripBg: 'rgba(0,0,0,0.08)',
  headBg: 'rgba(37,42,53,0.72)',
  toastBg: '#3a4150',
  switchOff: '#aab0ba',
  switchKnob: '#ffffff',
  scrim: 'rgba(0,0,0,0.45)',
};

export const light: ThemeTokens = {
  bg: '#f7f4ee',
  bgTop: '#fdfbf7',
  panel: '#fffefa',
  panel2: '#f2ede5',
  raise: '#fdfbf7',
  border: 'rgba(62,54,44,0.12)',
  borderStrong: 'rgba(62,54,44,0.17)',
  wash: 'rgba(62,54,44,0.05)',
  washStrong: 'rgba(62,54,44,0.075)',
  text: '#2b2621',
  dim: '#5b554d',
  faint: '#6b655c',
  title: '#2b2621',
  accent: '#4a6cf5',
  accent2: '#2f4bd6',
  accentStrong: '#3550d8',
  accentSoft: 'rgba(74,108,245,0.14)',
  accentLine: 'rgba(74,108,245,0.5)',
  onAccent: '#ffffff',
  green: '#0d6f45',
  red: '#bf2831',
  redWash: 'rgba(191,40,49,0.11)',
  redLine: 'rgba(191,40,49,0.45)',
  amber: '#8a4b08',
  amberWash: 'rgba(138,75,8,0.10)',
  amberLine: 'rgba(138,75,8,0.30)',
  userBg: '#f1eade',
  userInk: '#4a4235',
  codeBg: 'rgba(62,54,44,0.07)',
  codeInk: '#34429e',
  cmdBg: 'rgba(62,54,44,0.06)',
  cmdInk: '#2b2621',
  inputBg: 'rgba(62,54,44,0.04)',
  composerBg: 'rgba(62,54,44,0.035)',
  stripBg: 'rgba(62,54,44,0.025)',
  headBg: 'rgba(247,244,238,0.82)',
  toastBg: '#fffefa',
  switchOff: '#b7b0a4',
  switchKnob: '#ffffff',
  scrim: 'rgba(62,54,44,0.28)',
};

/** 用户可选的三种外观；`system` 跟随手机设置。 */
export type ThemeMode = 'system' | 'light' | 'dark';

export const themes: Record<'light' | 'dark', ThemeTokens> = { light, dark };
