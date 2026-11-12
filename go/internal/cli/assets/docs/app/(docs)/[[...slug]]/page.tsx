import { PageActions } from '@/components/docs/page-actions';
import { getMDXComponents } from '@/components/mdx';
import { source } from '@/lib/source';
import { ogImage, withBasePath } from '@/lib/shared';
import { DocsBody, DocsDescription, DocsPage, DocsTitle } from 'fumadocs-ui/layouts/docs/page';
import { createRelativeLink } from 'fumadocs-ui/mdx';
import type { Metadata } from 'next';
import { notFound } from 'next/navigation';

type PageParams = {
  slug?: string[];
};

type PageProps = {
  params: Promise<PageParams>;
};

// OpenGraph cards keyed by page slug.
const pageOgImages: Record<string, string> = {
  'installation/wendyos-nvidia-jetson-agx-thor': '/images/opengraph-thor.png',
  'installation/wendyos-qualcomm-dragonwing': '/images/opengraph-dragonwing.png',
  'integrations/ros2': '/images/opengraph-ros2.png',
  'guides/tutorials/mojo/hello-world': '/images/opengraph-mojo.png',
  'guides/tutorials/mojo/simple-web-server': '/images/opengraph-mojo.png',
  'guides/tutorials/mojo/max-graph': '/images/opengraph-max.png',
  'guides/tutorials/mojo/max-inference-service': '/images/opengraph-max.png',
};

const tutorialOgImages: Record<string, string> = {
  'guides/camera-exposure': '/images/og-tutorial-camera-exposure.png',
  'guides/fleet-deployment': '/images/og-tutorial-fleet-deployment.png',
  'guides/tutorials/python/amd-rocm-pytorch': '/images/og-tutorial-amd-rocm-pytorch.png',
  'guides/tutorials/python/robot-camera-opencv': '/images/og-tutorial-robot-camera-opencv.png',
  'guides/tutorials/python/g1-mujoco-simulation': '/images/og-tutorial-g1-mujoco-simulation.png',
  'guides/tutorials/python/raw-camera-frames': '/images/og-tutorial-raw-camera-frames.png',
  'guides/tutorials/python/cloud-udp-service': '/images/og-tutorial-cloud-udp-service.png',
};

const legacyRedirects: Record<string, string> = {
  'apps/wendy.json': '/reference/app-configuration/',
  'advanced/apps/wendy.json': '/reference/app-configuration/',
  'installation/wendyos-nvidia-jetson': '/installation/wendyos-nvidia-jetson-orin-nano/',
  'installation/wendy-agent-linux': '/installation/linux/',
  'installation/ubuntu': '/installation/linux/',
  'guides/tutorials/camera-exposure': '/guides/camera-exposure/',
  'guides/tutorials/fleet-deployment': '/guides/fleet-deployment/',
  'get-started/hardware': '/',
  'get-started/mac': '/installation/wendy-agent-macos/',
  'installation/developer-machine-setup': '/',
  'advanced/wendyos/mender-update': '/advanced/wendyos/ota-update/',
  'advanced/clients/wendy-cli/commands/analytics/disable': '/reference/cli/analytics/disable/',
  'advanced/clients/wendy-cli/commands/analytics/enable': '/reference/cli/analytics/enable/',
  'advanced/clients/wendy-cli/commands/cache/clear': '/reference/cli/cache/clear/',
  'guides/tutorials/typescript/simple-web-server': '/getting-started/app/',
  // Pages moved in the June 2026 restructure. Search engines and old links
  // still request these paths.
  'guides/cpp/camera': '/guides/tutorials/cpp/camera/',
  'guides/cpp/hello-world': '/guides/tutorials/cpp/hello-world/',
  'guides/cpp/simple-web-server': '/guides/tutorials/cpp/simple-web-server/',
  'guides/python/audio': '/guides/tutorials/python/audio/',
  'guides/python/bluetooth-discovery': '/guides/tutorials/python/bluetooth-discovery/',
  'guides/python/hello-world': '/guides/tutorials/python/hello-world/',
  'guides/python/simple-web-server': '/guides/tutorials/python/simple-web-server/',
  'guides/python/web-app': '/guides/tutorials/python/web-app/',
  'guides/python/webcam-streaming': '/guides/tutorials/python/webcam-streaming/',
  'guides/python/yolov8-webcam-detection': '/guides/tutorials/python/yolov8-webcam-detection/',
  'guides/rust/camera': '/guides/tutorials/rust/camera/',
  'guides/rust/hello-world': '/guides/tutorials/rust/hello-world/',
  'guides/rust/simple-web-server': '/guides/tutorials/rust/simple-web-server/',
  'guides/rust/web-app': '/guides/tutorials/rust/web-app/',
  'guides/swift/audio': '/guides/tutorials/swift/audio/',
  'guides/swift/camera': '/guides/tutorials/swift/camera/',
  'guides/swift/hello-world': '/guides/tutorials/swift/hello-world/',
  'guides/swift/simple-web-server': '/guides/tutorials/swift/simple-web-server/',
  'guides/swift/web-app': '/guides/tutorials/swift/web-app/',
  'guides/swift/wrapping-c-library': '/guides/tutorials/swift/wrapping-c-library/',
  'guides/typescript/camera': '/guides/tutorials/typescript/camera/',
  'guides/typescript/hello-world': '/guides/tutorials/typescript/hello-world/',
  'guides/typescript/web-app': '/guides/tutorials/typescript/web-app/',
  'guides/typescript/simple-web-server': '/getting-started/app/',
  'guides/device/cli-agent-relationship': '/device/cli-agent-relationship/',
  'guides/device/connecting-to-wifi': '/device/connecting-to-wifi/',
  'guides/device/discovering-devices': '/device/discovering-devices/',
  'guides/device/entitlements': '/device/entitlements/',
  'guides/device/managing-apps': '/device/managing-apps/',
  'guides/device/hardware': '/hardware/',
  'guides/device/hardware-bluetooth': '/hardware/bluetooth/',
  'guides/device/hardware-camera': '/hardware/camera/',
  'guides/device/hardware-gpu': '/hardware/gpu/',
  'guides/device/hardware-microphone': '/hardware/microphone/',
  'guides/device/hardware-speakers': '/hardware/speakers/',
  'guides/device/vscode-extension': '/remote-debugging/vscode-extension/',
  'installation/esp32': '/installation/wendy-lite-esp32/',
  'installation/linux-wendy-agent': '/installation/linux/',
  'buyers-guide': '/hardware/',
  'wendyos/flashing': '/advanced/wendyos/flashing/',
};

function getLegacyRedirect(slug?: string[]) {
  return legacyRedirects[slug?.join('/') ?? ''];
}

export default async function Page(props: PageProps) {
  const params = await props.params;
  const redirect = getLegacyRedirect(params.slug);
  if (redirect) {
    // A static export cannot answer with a 308, and permanentRedirect() only
    // redirects once the page's JavaScript runs. A meta refresh also works for
    // crawlers and readers without JavaScript. React hoists it into <head>.
    const target = withBasePath(redirect);
    return (
      <DocsPage>
        <meta httpEquiv="refresh" content={`0; url=${target}`} />
        <DocsBody>
          <p>
            This page has moved to <a href={target}>{target}</a>.
          </p>
        </DocsBody>
      </DocsPage>
    );
  }

  const page = source.getPage(params.slug);
  if (!page) notFound();

  const MDX = page.data.body;

  return (
    <DocsPage toc={page.data.toc} full={page.data.full}>
      <DocsTitle>{page.data.title}</DocsTitle>
      <DocsDescription>{page.data.description}</DocsDescription>
      <PageActions slug={params.slug?.join('/') ?? ''} />
      <DocsBody>
        <MDX
          components={getMDXComponents({
            a: createRelativeLink(source, page),
          })}
        />
      </DocsBody>
    </DocsPage>
  );
}

export function generateStaticParams() {
  const pages = source.generateParams();
  const pageSlugs = new Set(pages.map(({ slug }) => slug?.join('/') ?? ''));
  const redirectPages = Object.keys(legacyRedirects)
    .filter((slug) => !pageSlugs.has(slug))
    .map((slug) => ({ slug: slug.split('/') }));

  return [...pages, ...redirectPages];
}

export async function generateMetadata(props: PageProps): Promise<Metadata> {
  const params = await props.params;
  const redirect = getLegacyRedirect(params.slug);
  const pageSlug = redirect ? redirect.split('/').filter(Boolean) : params.slug;
  const slug = pageSlug?.join('/');
  const page = source.getPage(pageSlug);
  if (!page) notFound();

  const tutorialOgImage = slug ? tutorialOgImages[slug] : undefined;
  const pageOgImage = tutorialOgImage
    ? {
        url: withBasePath(tutorialOgImage),
        width: 1200,
        height: 630,
        type: 'image/png',
        alt: page.data.title,
      }
    : slug && slug in pageOgImages
      ? withBasePath(pageOgImages[slug])
      : ogImage;
  const pageUrl = withBasePath(page.url.endsWith('/') ? page.url : `${page.url}/`);

  return {
    title: page.data.title,
    description: page.data.description,
    alternates: {
      canonical: pageUrl,
    },
    openGraph: {
      type: 'website',
      siteName: 'WendyOS Docs',
      title: page.data.title,
      description: page.data.description,
      url: pageUrl,
      images: pageOgImage,
    },
    twitter: {
      card: 'summary_large_image',
      title: page.data.title,
      description: page.data.description,
      images: pageOgImage,
    },
  };
}
