export type AuthSession = {
  name: string;
  email: string;
  organization: string;
  profileWarning?: string;
  subject: string;
  tenant: string;
  issuer: string;
  principal: string;
  expires: string;
};
export const AUTH_CALLBACK_TYPE = "wendy-auth-callback";
