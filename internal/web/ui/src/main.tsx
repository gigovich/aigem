import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import { authenticate } from "./lib/protocol";
import "./index.css";

// Before the first render, not during it. The exchange decides how every
// request this page makes is authenticated - including the websockets, which
// are opened from an effect of the first render - and a page that started
// asking before it knew would put its token in the URL of each of them.
//
// It never rejects: a daemon that cannot issue a cookie is one this page still
// has to be able to drive with the token it was given.
void authenticate().finally(() => {
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
});
