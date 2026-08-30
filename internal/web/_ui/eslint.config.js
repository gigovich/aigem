import js from '@eslint/js'
import globals from 'globals'
import tseslint from 'typescript-eslint'
import reactHooks from 'eslint-plugin-react-hooks'
import jsxA11y from 'eslint-plugin-jsx-a11y'

export default tseslint.config(
  // node_modules is an ESLint default; dist is Vite's, and it is written
  // outside this directory anyway.
  { ignores: ['dist'] },
  js.configs.recommended,
  tseslint.configs.recommendedTypeChecked,
  reactHooks.configs.flat['recommended-latest'],
  // The design is dense with icon-only buttons - a gear, a chevron, an x, a
  // theme glyph. Each needs a name a screen reader can read, and enforcing that
  // now is far cheaper than retrofitting it across eight screens.
  jsxA11y.flatConfigs.recommended,
  {
    files: ['**/*.{ts,tsx}'],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
      // Type-aware rules. no-floating-promises alone justifies it: an unawaited
      // fetch chain in an effect is the defect this app is most likely to grow.
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
    },
  },
  // This file is not in either tsconfig project, so the type-aware rules have
  // no program to ask and would error rather than lint it.
  {
    files: ['**/*.js'],
    extends: [tseslint.configs.disableTypeChecked],
  },
)
