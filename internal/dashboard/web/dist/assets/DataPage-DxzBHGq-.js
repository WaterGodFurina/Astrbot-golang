import{P as y,u as _,c as u,G as k,f as h,A as t,B as c,F as g,g as b,h as f,ao as M,e as p,w as x,s as N,p as C,t as w,az as z,a as T,o as s,_ as B}from"./index-BHprFgx-.js";import{c as n}from"./createLucideIcon-YpG4zjeY.js";import{M as I}from"./message-square-text-BkHY59A7.js";/**
 * @license @lucide/vue v1.23.0 - ISC
 *
 * This source code is licensed under the ISC license.
 * See the LICENSE file in the root directory of this source tree.
 */const S=[["path",{d:"M5 21v-6",key:"1hz6c0"}],["path",{d:"M12 21V9",key:"uvy0l4"}],["path",{d:"M19 21V3",key:"11j9sm"}]],V=n("chart-no-axes-column-increasing",S);/**
 * @license @lucide/vue v1.23.0 - ISC
 *
 * This source code is licensed under the ISC license.
 * See the LICENSE file in the root directory of this source tree.
 */const q=[["path",{d:"M3 5h1",key:"1mv5vm"}],["path",{d:"M3 12h1",key:"lp3yf2"}],["path",{d:"M3 19h1",key:"w6f3n9"}],["path",{d:"M8 5h1",key:"1nxr5w"}],["path",{d:"M8 12h1",key:"1con00"}],["path",{d:"M8 19h1",key:"k7p10e"}],["path",{d:"M13 5h8",key:"a7qcls"}],["path",{d:"M13 12h8",key:"h98zly"}],["path",{d:"M13 19h8",key:"c3s6r1"}]],D=n("logs",q);/**
 * @license @lucide/vue v1.23.0 - ISC
 *
 * This source code is licensed under the ISC license.
 * See the LICENSE file in the root directory of this source tree.
 */const L=[["path",{d:"m10.586 5.414-5.172 5.172",key:"4mc350"}],["path",{d:"m18.586 13.414-5.172 5.172",key:"8c96vv"}],["path",{d:"M6 12h12",key:"8npq4p"}],["circle",{cx:"12",cy:"20",r:"2",key:"144qzu"}],["circle",{cx:"12",cy:"4",r:"2",key:"muu5ef"}],["circle",{cx:"20",cy:"12",r:"2",key:"1xzzfp"}],["circle",{cx:"4",cy:"12",r:"2",key:"1hvhnz"}]],P=n("waypoints",L),R={class:"data-workspace"},A={class:"data-tabs-scroll"},F=["aria-label"],$=["href","aria-selected","onClick"],j=y({__name:"DataPage",setup(E){const{t:e}=_(),r=T(),i=u(()=>[{value:"statistics",label:e("core.navigation.dataTabs.statistics"),routeName:"Stats",icon:V},{value:"conversations",label:e("core.navigation.dataTabs.conversations"),routeName:"Conversation",icon:I},{value:"logs",label:e("core.navigation.dataTabs.logs"),routeName:"Console",icon:D},{value:"trace",label:e("core.navigation.dataTabs.trace"),routeName:"Trace",icon:P}]),l=u(()=>String(r.meta.dataTab||"statistics"));return k(()=>r.meta.dataTab,d=>{const o=i.value.find(a=>a.value===d);o&&localStorage.setItem("data_last_tab",o.routeName)},{immediate:!0}),(d,o)=>(s(),h("div",R,[t("div",A,[t("nav",{class:"data-tabs",role:"tablist","aria-label":c(e)("core.navigation.data")},[(s(!0),h(g,null,b(i.value,a=>(s(),p(c(z),{key:a.value,custom:"",to:{name:a.routeName}},{default:x(({href:m,navigate:v})=>[t("a",{href:m,class:N(["data-tab",{"data-tab--active":l.value===a.value}]),role:"tab","aria-selected":l.value===a.value,onClick:v},[(s(),p(C(a.icon),{size:16,"stroke-width":1.8,"aria-hidden":"true"})),t("span",null,w(a.label),1)],10,$)]),_:2},1032,["to"]))),128))],8,F)]),f(c(M))]))}}),J=B(j,[["__scopeId","data-v-f162a78a"]]);export{J as default};
