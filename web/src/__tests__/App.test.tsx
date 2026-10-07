import { render, screen } from "@testing-library/react";
import { App } from "../App";

it("renders the module's landing page", () => {
  render(<App />);
  expect(screen.getByRole("heading", { name: "Streamlit apps" })).toBeInTheDocument();
});
