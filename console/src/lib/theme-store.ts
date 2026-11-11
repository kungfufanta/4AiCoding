import { create } from "zustand";

export type Theme = "dark" | "light";

interface ThemeState {
  theme: Theme;
  setTheme: (t: Theme) => void;
  toggle: () => void;
  /** sync store from the <html> class set by the no-flash boot script */
  hydrate: () => void;
}

function apply(t: Theme) {
  if (typeof document === "undefined") return;
  const el = document.documentElement;
  el.classList.toggle("light", t === "light");
  el.classList.toggle("dark", t === "dark");
  try {
    localStorage.setItem("sbx-theme", t);
  } catch {
    /* private mode */
  }
}

export const useTheme = create<ThemeState>((set, get) => ({
  theme: "dark",
  setTheme: (t) => {
    apply(t);
    set({ theme: t });
  },
  toggle: () => {
    const next: Theme = get().theme === "dark" ? "light" : "dark";
    apply(next);
    set({ theme: next });
  },
  hydrate: () => {
    const isLight =
      typeof document !== "undefined" &&
      document.documentElement.classList.contains("light");
    set({ theme: isLight ? "light" : "dark" });
  },
}));
