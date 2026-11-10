'use client';

import { CodeBlock } from 'fumadocs-ui/components/codeblock';
import { usePathname } from 'next/navigation';
import type { ComponentProps } from 'react';
import { cliCurlCommand, cliWingetCommand } from '@/components/docs/install-command';
import {
  type DocsAnalyticsEventParams,
  type DocsAnalyticsLocation,
  trackDocsAnalyticsEvent,
} from '@/lib/analytics-events';
import { cn } from '@/lib/cn';

type CliInstallCopyParams = DocsAnalyticsEventParams<'cli_install_copy'>;

// Pages whose CLI install code blocks send cli_install_copy, keyed by route
// without slashes. prepare-content serves .md pages under advanced/.
const cliInstallCopyLocations: Partial<Record<string, DocsAnalyticsLocation>> = {
  '': 'docs_quickstart_cli_install_command',
  'guides/llm-integration': 'docs_llm_integration_cli_install_command',
  'advanced/wendy-lite/getting-started': 'docs_wendy_lite_getting_started_cli_install_command',
};

const cliInstallCopies = new Map<string, Omit<CliInstallCopyParams, 'location'>>([
  [
    cliCurlCommand,
    { install_target: 'unix', install_variant: 'cli for macOS/linux', install_label: 'macOS/Linux CLI' },
  ],
  [
    cliWingetCommand,
    { install_target: 'windows', install_variant: 'cli for windows', install_label: 'windows CLI' },
  ],
]);

// Returns the params for the first line of a code block that is a CLI install
// command. Whitespace is collapsed, so aligned commands still match.
function cliInstallCopyParams(
  code: string,
  analyticsLocation: DocsAnalyticsLocation,
): CliInstallCopyParams | null {
  for (const line of code.split('\n')) {
    const copy = cliInstallCopies.get(line.trim().replace(/\s+/g, ' '));
    if (copy) return { ...copy, location: analyticsLocation };
  }
  return null;
}

// Renders every MDX code block. On the pages above, copying a code block that
// contains a CLI install command sends cli_install_copy. Elsewhere it is the
// default Fumadocs code block.
export function TrackedCodeBlock(props: ComponentProps<typeof CodeBlock>) {
  const analyticsLocation = cliInstallCopyLocations[usePathname().replace(/^\/|\/$/g, '')];
  if (!analyticsLocation) return <CodeBlock {...props} />;

  return (
    <CodeBlock
      {...props}
      // Same wrapper as the Fumadocs default; the copy button is its only child.
      Actions={({ className, children }) => (
        <div
          className={cn('empty:hidden', className)}
          onClick={(event) => {
            // The copy button copies the text of this code block's pre element.
            const code = event.currentTarget.closest('figure')?.querySelector('pre')?.textContent;
            const params = code ? cliInstallCopyParams(code, analyticsLocation) : null;
            if (params) trackDocsAnalyticsEvent('cli_install_copy', params);
          }}
        >
          {children}
        </div>
      )}
    />
  );
}
