import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { startThemeSync } from "./theme";
import "./index.css";

startThemeSync();

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
