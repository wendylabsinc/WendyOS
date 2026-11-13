import React from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import "../app/globals.css";
import "./desktop.css";

createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
