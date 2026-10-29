// Sets the theme before the app paints, so a light-mode user never sees a dark
// flash. A classic script in <head>, not inline: the CSP allows no inline script.
(function () {
  var theme = "dark";
  try {
    var stored = localStorage.getItem("aikonos.theme");
    if (stored === "dark" || stored === "light") {
      theme = stored;
    } else if (
      window.matchMedia &&
      window.matchMedia("(prefers-color-scheme: light)").matches
    ) {
      theme = "light";
    }
  } catch (_) {}
  document.documentElement.setAttribute("data-theme", theme);
})();
