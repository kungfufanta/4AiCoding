import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { PortalApp } from "./app/router";
import "./styles.css";

// Theme before first paint (CSP forbids an inline script in
// index.html, so this module does it just before React mounts).
const stored = localStorage.getItem("portal-theme");
const dark =
  stored === "dark" ||
  (stored === null && window.matchMedia("(prefers-color-scheme: dark)").matches);
document.documentElement.classList.toggle("dark", dark);

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <PortalApp />
  </StrictMode>,
);
