import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { Toaster } from "sonner";
import "@fontsource-variable/inter";
import "@fontsource-variable/jetbrains-mono";
import "sonner/dist/styles.css";
import "./globals.css";
import PlatformRoot from "@/components/platform/platform-root";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <PlatformRoot />
    <Toaster theme="system" position="bottom-right" richColors closeButton />
  </StrictMode>,
);
