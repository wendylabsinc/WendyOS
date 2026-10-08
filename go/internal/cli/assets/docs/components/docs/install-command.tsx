'use client';

import { Check, Copy } from 'lucide-react';
import { useState } from 'react';
import { trackDocsAnalyticsEvent, type DocsAnalyticsTrackingProps } from '@/lib/analytics-events';

export const cliCurlCommand = 'curl -fsSL https://install.wendy.dev/cli.sh | bash';
export const cliWingetCommand = 'winget install WendyLabs.Wendy --source winget';
export const agentCurlCommand = 'curl -fsSL https://install.wendy.dev/agent.sh | bash';
// SECURITY: This explicit WendyLabs tap trust is required when HOMEBREW_REQUIRE_TAP_TRUST is set;
// the linked Mac installation guide explains Homebrew version behavior and the narrower fallback.
export const agentMacBrewCommand = `brew tap wendylabsinc/tap
brew trust wendylabsinc/tap
brew install --cask wendy-agent`;

type CopyButtonProps = DocsAnalyticsTrackingProps & {
  text: string;
};

function CopyButton({ analyticsEventName, analyticsEventParams, text }: CopyButtonProps) {
  const [copied, setCopied] = useState(false);

  return (
    <button
      type="button"
      aria-label="Copy to clipboard"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text);
          if (analyticsEventName) {
            trackDocsAnalyticsEvent(analyticsEventName, analyticsEventParams);
          }
          setCopied(true);
          setTimeout(() => setCopied(false), 1500);
        } catch {
          /* clipboard unavailable */
        }
      }}
      className="shrink-0 border-l p-2.5 text-fd-muted-foreground transition-colors hover:bg-fd-accent hover:text-fd-accent-foreground"
    >
      {copied ? <Check className="size-4 text-green-500" /> : <Copy className="size-4" />}
    </button>
  );
}

type InstallCommandProps = DocsAnalyticsTrackingProps & {
  command: string;
  label?: string;
};

export function InstallCommand(props: InstallCommandProps) {
  const { label, command } = props;
  const copyButtonProps: CopyButtonProps = props.analyticsEventName
    ? {
        analyticsEventName: props.analyticsEventName,
        analyticsEventParams: props.analyticsEventParams,
        text: command,
      }
    : { text: command };

  return (
    <div className="mt-2">
      {label ? (
        <p className="mb-1 text-xs font-medium text-fd-muted-foreground">{label}</p>
      ) : null}
      <div className="flex items-stretch border bg-fd-secondary/40">
        <code className="min-w-0 flex-1 whitespace-pre-wrap break-all px-3 py-2.5 font-mono text-sm text-fd-foreground">
          {command}
        </code>
        <CopyButton {...copyButtonProps} />
      </div>
    </div>
  );
}
