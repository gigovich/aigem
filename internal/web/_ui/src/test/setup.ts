// Gives the DOM matchers every component test leans on - toBeInTheDocument,
// toHaveAttribute, toHaveTextContent - so assertions describe the rendered
// document rather than settling for toBeDefined().
import '@testing-library/jest-dom/vitest'
