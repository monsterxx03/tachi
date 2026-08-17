/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        // Transcript report palette (matches agent/transcript/render/templates/report.html)
        paper: '#fafaf8',
        paper2: '#f3f2ef',
        card: '#ffffff',
        line: '#e4e3df',
        line2: '#d4d3ce',
        ink: '#2c2c28',
        inkdim: '#6b6b65',
        muted: '#9d9d95',
        accent: '#4a9e6e',
        'accent-soft': '#e8f5ed',
        linkblue: '#3b82c4',
        linksoft: '#eff6ff',
        warn: '#b8860b',
        warnsoft: '#fefce8',
        danger: '#d9444f',
        dangersoft: '#fef2f2',
        violet: '#7c3aed',
        violetsft: '#f5f3ff',
        gold: '#a16207',
      },
      fontFamily: {
        mono: ["'SF Mono'", "'Fira Code'", "'Cascadia Code'", 'Menlo', 'monospace'],
        sans: ["-apple-system", "BlinkMacSystemFont", "'PingFang SC'", "'Segoe UI'", "system-ui", "sans-serif"],
      },
      boxShadow: {
        card: '0 1px 3px rgba(0,0,0,.06)',
      },
      borderRadius: {
        card: '6px',
      },
    },
  },
  plugins: [],
}