import React from "react";
import { createRoot } from "react-dom/client";
import Root from "./Root.jsx";
import { AuthProvider } from "./auth/AuthContext.jsx";
import { ToastProvider } from "./components/ui/Toast.jsx";
import "./globals.css";

createRoot(document.getElementById("root")).render(
  <React.StrictMode>
    <ToastProvider>
      <AuthProvider>
        <Root />
      </AuthProvider>
    </ToastProvider>
  </React.StrictMode>
);
