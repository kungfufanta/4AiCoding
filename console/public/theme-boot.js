// Applies the saved theme before first paint (a file, not an inline
// script: the CSP is script-src 'self'). Dark is the brand default.
try {
  var t = localStorage.getItem("sbx-theme") || localStorage.getItem("console-theme");
  if (t === "light") document.documentElement.classList.add("light");
  else document.documentElement.classList.add("dark");
} catch (e) {
  document.documentElement.classList.add("dark");
}
