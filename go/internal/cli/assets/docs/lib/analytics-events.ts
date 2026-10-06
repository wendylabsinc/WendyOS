export type DocsInstallCopyTarget = 'unix' | 'windows' | 'agent-linux' | 'agent-macos';
export type DocsInstallCopyVariant =
  | 'cli for macOS/linux'
  | 'cli for windows'
  | 'wendy-agent for Linux'
  | 'wendy-agent for Mac';
export type DocsInstallCopyLabel =
  | 'macOS/Linux CLI'
  | 'windows CLI'
  | 'wendy-agent for Linux'
  | 'wendy-agent for Mac';
export type DocsAnalyticsLocation =
  | 'docs_get_started_cli_install_command'
  | 'docs_get_started_agent_install_command'
  | 'docs_install_scripts_dialog';

export type DocsAnalyticsEvents = {
  cli_install_copy: {
    install_target: DocsInstallCopyTarget;
    install_variant: DocsInstallCopyVariant;
    install_label: DocsInstallCopyLabel;
    location: DocsAnalyticsLocation;
  };
};

export type DocsAnalyticsEventName = keyof DocsAnalyticsEvents;
export type DocsAnalyticsEventParams<T extends DocsAnalyticsEventName = DocsAnalyticsEventName> =
  DocsAnalyticsEvents[T];

export type DocsAnalyticsTrackingProps<T extends DocsAnalyticsEventName = DocsAnalyticsEventName> =
  | {
      analyticsEventName: T;
      analyticsEventParams: DocsAnalyticsEventParams<T>;
    }
  | {
      analyticsEventName?: undefined;
      analyticsEventParams?: undefined;
    };

declare global {
  interface Window {
    gtag?: (...args: unknown[]) => void;
  }
}

export function trackDocsAnalyticsEvent<T extends DocsAnalyticsEventName>(
  eventName: T,
  params: DocsAnalyticsEventParams<T>,
) {
  if (typeof window === 'undefined' || typeof window.gtag !== 'function') return;

  window.gtag('event', eventName, {
    ...params,
    // Docs install-copy events intentionally share the marketing-site GA4 schema.
    event_category: 'marketing',
  });
}
