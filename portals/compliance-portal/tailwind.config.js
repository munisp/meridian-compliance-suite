/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        sand: {
          50: '#faf8f5', 100: '#f3efe8', 200: '#e6ded1', 300: '#d4c7b2',
          400: '#bda98b', 500: '#a68d6d', 600: '#8a7157', 700: '#6f5a46',
          800: '#584739', 900: '#41352c',
        },
        clay: { 500: '#b07050', 600: '#96593d', 700: '#7a4630' },
        moss: { 500: '#7d8a68', 600: '#657353', 700: '#505c42' },
      },
      fontFamily: {
        sans: ['Inter', 'ui-sans-serif', 'system-ui', 'sans-serif'],
      },
    },
  },
  plugins: [],
}
