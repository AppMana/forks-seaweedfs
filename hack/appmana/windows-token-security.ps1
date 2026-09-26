Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using System.Security.Principal;
using System.Security.AccessControl;
public class TokenSecurityProbe {
 [DllImport("advapi32.dll", SetLastError=true)] static extern bool GetTokenInformation(IntPtr token, int kind, IntPtr buffer, int size, out int needed);
 static IntPtr Info(int kind) {
  int needed; IntPtr token=WindowsIdentity.GetCurrent().Token;
  GetTokenInformation(token,kind,IntPtr.Zero,0,out needed);
  IntPtr p=Marshal.AllocHGlobal(needed);
  if(!GetTokenInformation(token,kind,p,needed,out needed)) { Marshal.FreeHGlobal(p); throw new System.ComponentModel.Win32Exception(); }
  return p;
 }
 public static string Read() {
  IntPtr owner=IntPtr.Zero, group=IntPtr.Zero, dacl=IntPtr.Zero;
  try {
   owner=Info(4); group=Info(5); dacl=Info(6);
   var o=new SecurityIdentifier(Marshal.ReadIntPtr(owner));
   var g=new SecurityIdentifier(Marshal.ReadIntPtr(group));
   IntPtr acl=Marshal.ReadIntPtr(dacl);
   if(acl==IntPtr.Zero) return new RawSecurityDescriptor(ControlFlags.DiscretionaryAclPresent,o,g,null,null).GetSddlForm(AccessControlSections.All);
   byte[] bytes=new byte[(ushort)Marshal.ReadInt16(acl,2)];
   Marshal.Copy(acl,bytes,0,bytes.Length);
   return new RawSecurityDescriptor(ControlFlags.DiscretionaryAclPresent,o,g,null,new RawAcl(bytes,0)).GetSddlForm(AccessControlSections.All);
  } finally { Marshal.FreeHGlobal(owner); Marshal.FreeHGlobal(group); Marshal.FreeHGlobal(dacl); }
 }
}
'@
[TokenSecurityProbe]::Read()
