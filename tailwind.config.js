/** @type {import('tailwindcss').Config} */
module.exports = {
  content: [
    "./web/views/**/*.templ",
    "./web/views/**/*_templ.go",
  ],
  theme: {
    extend: {
      colors: {
        relay: {
          bg: "oklch(0.16 0.012 175)",
          card: "oklch(0.2 0.014 175)",
          fg: "oklch(0.96 0.01 160)",
          primary: "oklch(0.78 0.19 152)",
          primaryfg: "oklch(0.16 0.02 160)",
          muted: "oklch(0.25 0.015 175)",
          mutedfg: "oklch(0.7 0.02 170)",
          danger: "oklch(0.66 0.21 25)",
          warning: "oklch(0.82 0.16 85)",
        },
      },
      fontFamily: {
        display: ['"Bricolage Grotesque"', "system-ui", "sans-serif"],
      },
    },
  },
  plugins: [],
};
